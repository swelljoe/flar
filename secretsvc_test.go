package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHelloReplyGolden pins the D-Bus wire format independently of the decoder,
// so a self-consistent encode/decode bug can't pass silently. The bytes were
// derived by hand from the D-Bus spec for a METHOD_RETURN carrying the string
// ":1.1" with serial=1 and reply_serial=1.
func TestHelloReplyGolden(t *testing.T) {
	got := reply(&inMessage{serial: 1}, 1, "s", func(e *enc) { e.str(":1.1") })
	want := []byte{
		// fixed header: endian 'l', METHOD_RETURN(2), flags 1, version 1
		0x6c, 0x02, 0x01, 0x01,
		0x09, 0x00, 0x00, 0x00, // body length = 9
		0x01, 0x00, 0x00, 0x00, // serial = 1
		0x0f, 0x00, 0x00, 0x00, // header-fields array length = 15
		// REPLY_SERIAL (code 5), variant sig "u", value 1
		0x05, 0x01, 0x75, 0x00, 0x01, 0x00, 0x00, 0x00,
		// SIGNATURE (code 8), variant sig "g", value "s"
		0x08, 0x01, 0x67, 0x00, 0x01, 0x73, 0x00,
		0x00,                                     // pad to 8 before body
		0x04, 0x00, 0x00, 0x00, ':', '1', '.', '1', 0x00, // body: string ":1.1"
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Hello reply bytes mismatch\n got: % x\nwant: % x", got, want)
	}
}

// TestSecretServiceEndToEnd exercises the full path over a real socket: SASL
// handshake, Hello, OpenSession, and GetSecret — the exact sequence agy's
// go-keyring client drives — and verifies the served secret round-trips
// byte-for-byte, including embedded NUL and high bytes.
func TestSecretServiceEndToEnd(t *testing.T) {
	secret := "header.\x00\xff.payload-505-ish"
	sock := filepath.Join(t.TempDir(), "s.sock")

	go func() { _ = RunSecretService(sock, secret) }()
	conn := dialWhenReady(t, sock)
	defer conn.Close()
	br := bufio.NewReader(conn)

	// SASL: NUL credential byte, then AUTH EXTERNAL, then BEGIN.
	if _, err := conn.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "AUTH EXTERNAL %x\r\n", []byte("1000"))
	line, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "OK ") {
		t.Fatalf("expected OK, got %q err=%v", line, err)
	}
	io.WriteString(conn, "BEGIN\r\n")

	call(t, conn, br, 1, "/org/freedesktop/DBus", "org.freedesktop.DBus", "Hello", "", nil)

	call(t, conn, br, 2, pathSecrets, "org.freedesktop.Secret.Service", "OpenSession", "sv",
		func(e *enc) {
			e.str("plain")
			e.variant("s", func(e *enc) { e.str("") })
		})

	reply := call(t, conn, br, 3, pathItem, "org.freedesktop.Secret.Item", "GetSecret", "o",
		func(e *enc) { e.objPath(pathSession) })

	if reply.typ != msgMethodReply {
		t.Fatalf("GetSecret reply type = %d, want %d", reply.typ, msgMethodReply)
	}
	if got := decodeSecretValue(reply); got != secret {
		t.Fatalf("secret mismatch\n got: %q\nwant: %q", got, secret)
	}
}

func TestUnknownMethodReturnsError(t *testing.T) {
	secret := "x"
	sock := filepath.Join(t.TempDir(), "s.sock")
	go func() { _ = RunSecretService(sock, secret) }()
	conn := dialWhenReady(t, sock)
	defer conn.Close()
	br := bufio.NewReader(conn)

	conn.Write([]byte{0})
	fmt.Fprintf(conn, "AUTH EXTERNAL %x\r\n", []byte("1000"))
	br.ReadString('\n')
	io.WriteString(conn, "BEGIN\r\n")

	reply := call(t, conn, br, 1, pathSecrets, "org.freedesktop.Secret.Service", "Nonexistent", "", nil)
	if reply.typ != msgError {
		t.Fatalf("unknown method reply type = %d, want error(%d)", reply.typ, msgError)
	}
}

func TestBodyStrings(t *testing.T) {
	e := &enc{}
	e.str("org.freedesktop.Secret.Service")
	e.str("Collections")
	m := &inMessage{raw: e.buf, bodyStart: 0}
	got := m.bodyStrings(2)
	if len(got) != 2 || got[0] != "org.freedesktop.Secret.Service" || got[1] != "Collections" {
		t.Fatalf("bodyStrings = %q", got)
	}
}

func TestBodyObjPaths(t *testing.T) {
	e := &enc{}
	e.arrayObjPaths([]string{"/org/freedesktop/secrets/collection/login", "/org/freedesktop/secrets/item/1"})
	m := &inMessage{raw: e.buf, bodyStart: 0}
	got := m.bodyObjPaths()
	if len(got) != 2 || got[0] != "/org/freedesktop/secrets/collection/login" || got[1] != "/org/freedesktop/secrets/item/1" {
		t.Fatalf("bodyObjPaths = %q", got)
	}
}

func TestExtractAgyTokenMissingToolIsGraceful(t *testing.T) {
	// With an empty PATH, secret-tool cannot be found; extraction must return
	// ("", nil) so agy simply falls back to its normal login.
	t.Setenv("PATH", "")
	tok, err := extractAgyToken()
	if err != nil || tok != "" {
		t.Fatalf("extractAgyToken with no secret-tool = (%q, %v), want (\"\", nil)", tok, err)
	}
}

// --- test helpers ---

func dialWhenReady(t *testing.T, sock string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", sock); err == nil {
			return conn
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("secret service socket %s never became ready", sock)
	return nil
}

// call sends a method call and returns the decoded reply.
func call(t *testing.T, conn net.Conn, br *bufio.Reader, serial uint32, path, iface, member, bodySig string, body func(*enc)) *inMessage {
	t.Helper()
	if _, err := conn.Write(buildCall(serial, path, iface, member, bodySig, body)); err != nil {
		t.Fatal(err)
	}
	msg, err := readMessage(br)
	if err != nil {
		t.Fatalf("read reply for %s.%s: %v", iface, member, err)
	}
	return msg
}

// buildCall marshals a METHOD_CALL message.
func buildCall(serial uint32, path, iface, member, bodySig string, body func(*enc)) []byte {
	e := &enc{}
	e.byte('l')
	e.byte(msgMethodCall)
	e.byte(0) // flags
	e.byte(1) // version
	bodyLenPos := len(e.buf)
	e.u32(0)
	e.u32(serial)
	e.array(8, func(e *enc) {
		field := func(code byte, sig string, val func(*enc)) {
			e.pad(8)
			e.byte(code)
			e.variant(sig, val)
		}
		field(fieldPath, "o", func(e *enc) { e.objPath(path) })
		field(fieldInterface, "s", func(e *enc) { e.str(iface) })
		field(fieldMember, "s", func(e *enc) { e.str(member) })
		if bodySig != "" {
			field(fieldSignature, "g", func(e *enc) { e.sig(bodySig) })
		}
	})
	e.pad(8)
	bodyStart := len(e.buf)
	if body != nil {
		body(e)
	}
	binary.LittleEndian.PutUint32(e.buf[bodyLenPos:], uint32(len(e.buf)-bodyStart))
	return e.buf
}

// decodeSecretValue parses the value field out of a GetSecret reply body,
// signature (oayays): (session, parameters, value, content_type).
func decodeSecretValue(m *inMessage) string {
	off := m.bodyStart
	readStr := func() { // o or s
		off = align4(off)
		l := int(binary.LittleEndian.Uint32(m.raw[off : off+4]))
		off += 4 + l + 1
	}
	readAy := func() []byte {
		off = align4(off)
		l := int(binary.LittleEndian.Uint32(m.raw[off : off+4]))
		off += 4
		b := m.raw[off : off+l]
		off += l
		return b
	}
	readStr()        // session object path
	readAy()         // parameters
	v := readAy()    // value
	return string(v) // content_type ignored
}

package main

// Minimal D-Bus Secret Service, self-contained (no external bus, no gnome-keyring).
//
// agy authenticates by reading its token from the OS keyring via the
// zalando/go-keyring library, which speaks the freedesktop Secret Service API
// over the D-Bus session bus. The sandbox has no session bus, so that fails.
//
// This implements just enough of the protocol to serve exactly one secret to a
// single client: flar acts as both the bus (answering Hello) and the Secret
// Service. It is pointed at a private Unix socket via DBUS_SESSION_BUS_ADDRESS,
// so the agent can reach this one secret and nothing else.

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
)

// agy's keyring identifiers (zalando/go-keyring: service + username) and the
// filename flar uses to hand the extracted token to the in-sandbox service.
const (
	agyKeyringService  = "gemini"
	agyKeyringUsername = "antigravity"
	agySecretFile      = "agy-secret"
	// agyBusSocket is the private Secret Service socket inside the sandbox.
	agyBusSocket = "/run/flar-secrets.sock"
)

// extractAgyToken reads agy's OAuth token from the host keyring via secret-tool.
// Returns an empty string (no error) if the tool or item is absent.
func extractAgyToken() (string, error) {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return "", nil
	}
	out, err := exec.Command("secret-tool", "lookup",
		"service", agyKeyringService, "username", agyKeyringUsername).Output()
	if err != nil {
		return "", nil // not found / locked: fall through, agy will prompt
	}
	return string(out), nil
}

// Object paths served by the fake Secret Service.
const (
	pathSecrets    = "/org/freedesktop/secrets"
	pathCollection = "/org/freedesktop/secrets/collection/login"
	pathItem       = "/org/freedesktop/secrets/item/1"
	pathSession    = "/org/freedesktop/secrets/session/1"
)

// RunSecretService listens on socketPath and serves `secret` to the first
// (and only) client. Blocks until the connection ends.
func RunSecretService(socketPath, secret string) error {
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("secretsvc listen: %w", err)
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go serveConn(conn, secret)
	}
}

func serveConn(conn net.Conn, secret string) {
	defer conn.Close()
	br := bufio.NewReader(conn)

	if err := saslServer(br, conn); err != nil {
		dbgf("sasl: %v", err)
		return
	}

	var outSerial uint32
	for {
		msg, err := readMessage(br)
		if err != nil {
			if err != io.EOF {
				dbgf("read: %v", err)
			}
			return
		}
		if msg.typ != msgMethodCall {
			continue
		}
		outSerial++
		reply := dispatch(msg, secret, outSerial)
		if reply == nil {
			continue
		}
		if _, err := conn.Write(reply); err != nil {
			dbgf("write: %v", err)
			return
		}
	}
}

// --- SASL handshake (server side) ---

// A fixed server GUID (32 hex chars) is fine for a single-client private bus.
const serverGUID = "0123456789abcdef0123456789abcdef"

func saslServer(br *bufio.Reader, w io.Writer) error {
	// D-Bus clients send a leading NUL credential byte before SASL text.
	if b, err := br.ReadByte(); err != nil {
		return err
	} else if b != 0 {
		return fmt.Errorf("expected NUL, got %#x", b)
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		dbgf("sasl <- %q", line)
		switch {
		case strings.HasPrefix(line, "AUTH EXTERNAL"):
			fmt.Fprintf(w, "OK %s\r\n", serverGUID)
		case strings.HasPrefix(line, "AUTH"):
			io.WriteString(w, "REJECTED EXTERNAL\r\n")
		case line == "NEGOTIATE_UNIX_FD":
			// We never pass file descriptors.
			io.WriteString(w, "ERROR\r\n")
		case line == "BEGIN":
			return nil
		case line == "CANCEL":
			io.WriteString(w, "REJECTED EXTERNAL\r\n")
		default:
			io.WriteString(w, "ERROR\r\n")
		}
	}
}

// --- Incoming message parsing ---

const (
	msgMethodCall  = 1
	msgMethodReply = 2
	msgError       = 3
)

// D-Bus header field codes.
const (
	fieldPath        = 1
	fieldInterface   = 2
	fieldMember      = 3
	fieldErrorName   = 4
	fieldReplySerial = 5
	fieldSignature   = 8
)

type inMessage struct {
	typ       byte
	serial    uint32
	path      string
	iface     string
	member    string
	raw       []byte
	bodyStart int
}

func align8(n int) int { return (n + 7) &^ 7 }

// readMessage reads one full D-Bus message frame from r.
func readMessage(r io.Reader) (*inMessage, error) {
	hdr := make([]byte, 16)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	if hdr[0] != 'l' {
		return nil, fmt.Errorf("unsupported endianness %#x", hdr[0])
	}
	bodyLen := binary.LittleEndian.Uint32(hdr[4:8])
	serial := binary.LittleEndian.Uint32(hdr[8:12])
	hdrArrayLen := binary.LittleEndian.Uint32(hdr[12:16])

	headerEnd := 16 + int(hdrArrayLen)
	bodyStart := align8(headerEnd)
	total := bodyStart + int(bodyLen)

	rest := make([]byte, total-16)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, err
	}
	raw := append(hdr, rest...)

	m := &inMessage{typ: hdr[1], serial: serial, raw: raw, bodyStart: bodyStart}
	parseHeaderFields(m, raw[16:headerEnd])
	dbgf("call <- path=%q iface=%q member=%q serial=%d", m.path, m.iface, m.member, m.serial)
	return m, nil
}

// parseHeaderFields walks the a(yv) header-field array. Offsets are absolute
// (relative to message start) because alignment is defined that way.
func parseHeaderFields(m *inMessage, fields []byte) {
	off := 16 // absolute offset of the first field
	end := 16 + len(fields)
	get := func(i int) byte { return m.raw[i] }

	for off < end {
		off = align8(off)
		if off >= end {
			break
		}
		code := get(off)
		off++
		// variant: signature (1-byte len + sig + NUL) then value
		sigLen := int(get(off))
		off++
		sig := string(m.raw[off : off+sigLen])
		off += sigLen + 1 // skip sig bytes + NUL

		switch sig {
		case "s", "o":
			off = align4(off)
			n := int(binary.LittleEndian.Uint32(m.raw[off : off+4]))
			off += 4
			val := string(m.raw[off : off+n])
			off += n + 1
			switch code {
			case fieldPath:
				m.path = val
			case fieldInterface:
				m.iface = val
			case fieldMember:
				m.member = val
			}
		case "g":
			n := int(get(off))
			off += n + 2 // len byte + sig + NUL
		case "u":
			off = align4(off) + 4
		default:
			// Unknown field type; we cannot safely skip, so stop.
			return
		}
	}
}

func align4(n int) int { return (n + 3) &^ 3 }

// bodyObjPaths decodes a leading array-of-object-paths (ao) argument.
func (m *inMessage) bodyObjPaths() []string {
	off := align4(m.bodyStart)
	if off+4 > len(m.raw) {
		return nil
	}
	arrLen := int(binary.LittleEndian.Uint32(m.raw[off : off+4]))
	off += 4
	end := off + arrLen
	var out []string
	for off < end {
		off = align4(off)
		if off+4 > len(m.raw) {
			break
		}
		l := int(binary.LittleEndian.Uint32(m.raw[off : off+4]))
		off += 4
		if off+l > len(m.raw) {
			break
		}
		out = append(out, string(m.raw[off:off+l]))
		off += l + 1
	}
	return out
}

// bodyStrings decodes the first n STRING/OBJECT_PATH arguments from the body.
func (m *inMessage) bodyStrings(n int) []string {
	off := m.bodyStart
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		off = align4(off)
		if off+4 > len(m.raw) {
			break
		}
		l := int(binary.LittleEndian.Uint32(m.raw[off : off+4]))
		off += 4
		if off+l > len(m.raw) {
			break
		}
		out = append(out, string(m.raw[off:off+l]))
		off += l + 1
	}
	return out
}

// --- Dispatch ---

// dispatch handles one method call and returns the encoded reply, or nil if no
// reply should be sent.
func dispatch(m *inMessage, secret string, serial uint32) []byte {
	switch m.iface {
	case "org.freedesktop.DBus":
		switch m.member {
		case "Hello":
			return reply(m, serial, "s", func(e *enc) { e.str(":1.1") })
		case "AddMatch", "RemoveMatch":
			return reply(m, serial, "", nil)
		case "GetNameOwner":
			return reply(m, serial, "s", func(e *enc) { e.str(":1.1") })
		}
	case "org.freedesktop.DBus.Peer":
		switch m.member {
		case "Ping":
			return reply(m, serial, "", nil)
		case "GetMachineId":
			return reply(m, serial, "s", func(e *enc) { e.str(serverGUID) })
		}
	case "org.freedesktop.Secret.Service":
		switch m.member {
		case "OpenSession":
			return reply(m, serial, "vo", func(e *enc) {
				e.variant("s", func(e *enc) { e.str("") })
				e.objPath(pathSession)
			})
		case "Unlock", "Lock":
			// Echo back the requested paths as unlocked, with no prompt.
			paths := m.bodyObjPaths()
			return reply(m, serial, "aoo", func(e *enc) {
				e.arrayObjPaths(paths)
				e.objPath("/")
			})
		case "SearchItems":
			// (unlocked []objpath, locked []objpath)
			return reply(m, serial, "aoao", func(e *enc) {
				e.arrayObjPaths([]string{pathItem})
				e.arrayObjPaths(nil)
			})
		case "ReadAlias":
			return reply(m, serial, "o", func(e *enc) { e.objPath(pathCollection) })
		case "GetSecrets":
			// a{o(oayays)} — map of item -> secret. Serve our one item.
			return reply(m, serial, "a{o(oayays)}", func(e *enc) {
				e.arrayDictItemSecret(pathItem, secret)
			})
		}
	case "org.freedesktop.Secret.Collection":
		switch m.member {
		case "SearchItems":
			return reply(m, serial, "ao", func(e *enc) {
				e.arrayObjPaths([]string{pathItem})
			})
		}
	case "org.freedesktop.Secret.Item":
		switch m.member {
		case "GetSecret":
			return reply(m, serial, "(oayays)", func(e *enc) {
				e.secretStruct(secret)
			})
		}
	case "org.freedesktop.Secret.Session":
		switch m.member {
		case "Close":
			return reply(m, serial, "", nil)
		}
	case "org.freedesktop.DBus.Properties":
		switch m.member {
		case "Get":
			// Args are (interface, property). Return type depends on property.
			args := m.bodyStrings(2)
			prop := ""
			if len(args) == 2 {
				prop = args[1]
			}
			switch prop {
			case "Collections":
				return reply(m, serial, "v", func(e *enc) {
					e.variant("ao", func(e *enc) { e.arrayObjPaths([]string{pathCollection}) })
				})
			case "Locked":
				return reply(m, serial, "v", func(e *enc) {
					e.variant("b", func(e *enc) { e.u32(0) })
				})
			case "Label":
				return reply(m, serial, "v", func(e *enc) {
					e.variant("s", func(e *enc) { e.str("agy") })
				})
			default:
				return reply(m, serial, "v", func(e *enc) {
					e.variant("s", func(e *enc) { e.str("") })
				})
			}
		}
	}
	dbgf("UNHANDLED %s.%s", m.iface, m.member)
	return errorReply(m, serial, "org.freedesktop.DBus.Error.UnknownMethod",
		fmt.Sprintf("no such method %s.%s", m.iface, m.member))
}

// --- Reply encoding ---

type enc struct{ buf []byte }

func (e *enc) pad(n int) {
	for len(e.buf)%n != 0 {
		e.buf = append(e.buf, 0)
	}
}
func (e *enc) byte(b byte) { e.buf = append(e.buf, b) }
func (e *enc) u32(v uint32) {
	e.pad(4)
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	e.buf = append(e.buf, b[:]...)
}
func (e *enc) str(s string) { // STRING or OBJECT_PATH
	e.u32(uint32(len(s)))
	e.buf = append(e.buf, s...)
	e.buf = append(e.buf, 0)
}
func (e *enc) objPath(s string) { e.str(s) }
func (e *enc) sig(s string) { // SIGNATURE
	e.byte(byte(len(s)))
	e.buf = append(e.buf, s...)
	e.buf = append(e.buf, 0)
}
func (e *enc) ay(data []byte) { // array of bytes
	e.u32(uint32(len(data)))
	e.buf = append(e.buf, data...)
}
func (e *enc) variant(signature string, val func(*enc)) {
	e.sig(signature)
	if val != nil {
		val(e)
	}
}

// array writes an array with the given element alignment, backpatching its
// byte length. Element bytes are counted from after the alignment padding.
func (e *enc) array(elemAlign int, elems func(*enc)) {
	e.u32(0)
	lenPos := len(e.buf) - 4
	e.pad(elemAlign)
	start := len(e.buf)
	if elems != nil {
		elems(e)
	}
	binary.LittleEndian.PutUint32(e.buf[lenPos:], uint32(len(e.buf)-start))
}

func (e *enc) arrayObjPaths(paths []string) {
	e.array(4, func(e *enc) {
		for _, p := range paths {
			e.objPath(p)
		}
	})
}

// secretStruct writes the Secret struct: (objpath session, ay params, ay value, string content_type)
func (e *enc) secretStruct(secret string) {
	e.pad(8)
	e.objPath(pathSession)
	e.ay(nil)
	e.ay([]byte(secret))
	e.str("text/plain")
}

// arrayDictItemSecret writes a{o(oayays)} with a single entry.
func (e *enc) arrayDictItemSecret(item, secret string) {
	e.array(8, func(e *enc) {
		e.pad(8) // dict entry
		e.objPath(item)
		e.secretStruct(secret)
	})
}

// reply builds a METHOD_RETURN for m with the given body signature/body.
func reply(m *inMessage, serial uint32, bodySig string, body func(*enc)) []byte {
	return encodeMessage(msgMethodReply, serial, m.serial, "", bodySig, body)
}

func errorReply(m *inMessage, serial uint32, name, msg string) []byte {
	return encodeMessage(msgError, serial, m.serial, name, "s", func(e *enc) { e.str(msg) })
}

// encodeMessage marshals a full reply message from offset 0.
func encodeMessage(typ byte, serial, replySerial uint32, errName, bodySig string, body func(*enc)) []byte {
	e := &enc{}
	e.byte('l')
	e.byte(typ)
	e.byte(1) // flags: NO_REPLY_EXPECTED
	e.byte(1) // protocol version
	bodyLenPos := len(e.buf)
	e.u32(0)      // body length (backpatched)
	e.u32(serial) // this message's serial

	// Header fields: a(yv)
	e.array(8, func(e *enc) {
		// REPLY_SERIAL (u)
		e.pad(8)
		e.byte(fieldReplySerial)
		e.variant("u", func(e *enc) { e.u32(replySerial) })
		if errName != "" {
			e.pad(8)
			e.byte(fieldErrorName)
			e.variant("s", func(e *enc) { e.str(errName) })
		}
		if bodySig != "" {
			e.pad(8)
			e.byte(fieldSignature)
			e.variant("g", func(e *enc) { e.sig(bodySig) })
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

// --- debug ---

var secretDebug = os.Getenv("FLAR_SECRETSVC_DEBUG") != ""

func dbgf(format string, args ...any) {
	if secretDebug {
		fmt.Fprintf(os.Stderr, "[secretsvc] "+format+"\n", args...)
	}
}

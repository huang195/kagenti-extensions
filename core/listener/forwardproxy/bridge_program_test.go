package forwardproxy

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// Tests for the TLS bridge's per-program rules
// (docs/superpowers/specs/2026-10-09-tls-bridge-per-program-design.md): a refusal is
// remembered against the program that refused, not the host it was talking to, so a
// different program on the same host is still bridged.

const (
	exeClaude = "/bin/claude"
	exeHelm   = "/opt/homebrew/bin/helm"
	exeCurl   = "/usr/bin/curl"
	exePython = "/usr/bin/python3"
)

// bridgeScene is a forward proxy with process attribution and a TLS bridge in front of
// a TLS origin, served the way cortex serves it.
type bridgeScene struct {
	proxyURL, backendURL, target string
	engine                       *tlsbridge.Engine
	bridgeCA                     *x509.Certificate
	// trusting holds the bridge CA and the origin's certificate: a client the bridge can
	// read. refusing holds only the origin's: a client that refuses the bridge's leaf but
	// completes a tunnelled handshake to the real server.
	trusting, refusing *x509.CertPool
}

func newBridgeScene(t *testing.T, store *session.Store, procs *fakeProcs) *bridgeScene {
	t.Helper()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("OK"))
	}))
	t.Cleanup(origin.Close)
	originCAPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
	src, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	up, err := tlsbridge.NewUpstreamClient(originCAPEM, false)
	if err != nil {
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	sc := &bridgeScene{target: strings.TrimPrefix(origin.URL, "https://")}
	sc.engine = &tlsbridge.Engine{
		Decision: mustBridgeDecision(t, portOf(sc.target)),
		Term:     tlsbridge.NewTerminator(tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})),
		Skip:     tlsbridge.NewSkipSet(),
		Programs: tlsbridge.NewProgramSkipSet(),
		Upstream: up,
		CAPEM:    src.CACertPEM(),
	}
	sc.bridgeCA, _ = src.Issuer()
	sc.trusting = x509.NewCertPool()
	sc.trusting.AddCert(sc.bridgeCA)
	sc.trusting.AddCert(origin.Certificate())
	sc.refusing = x509.NewCertPool()
	sc.refusing.AddCert(origin.Certificate())
	sc.proxyURL, sc.backendURL, _ = newProcessProxy(t, store, procs, func(s *Server) { s.TLSBridge = sc.engine })
	return sc
}

// handshake opens a CONNECT to the origin as pid and completes TLS over it trusting
// roots. bridged reports whether the leaf it was shown is the bridge's, which tells a
// decrypted connection from a tunnelled one.
func (sc *bridgeScene) handshake(t *testing.T, procs *fakeProcs, pid int32, roots *x509.CertPool) (bridged bool, err error) {
	t.Helper()
	raw, _, resp := connectAs(t, procs, sc.proxyURL, sc.target, pid)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT answered %d", resp.StatusCode)
	}
	tc := tls.Client(raw, &tls.Config{ServerName: "127.0.0.1", RootCAs: roots})
	if err := tc.Handshake(); err != nil {
		return false, err
	}
	defer func() { _ = tc.Close() }()
	return bytes.Equal(tc.ConnectionState().PeerCertificates[0].RawIssuer, sc.bridgeCA.RawSubject), nil
}

// tunnelReasons is the reason on every tunnel-open row recorded under id, in order.
func tunnelReasons(store *session.Store, id string) []pipeline.TunnelReason {
	v := store.View(id)
	if v == nil {
		return nil
	}
	var out []pipeline.TunnelReason
	for _, e := range v.Events {
		if e.Tunnel && e.Phase == pipeline.SessionRequest && e.TunnelReason != "" {
			out = append(out, e.TunnelReason)
		}
	}
	return out
}

// The headline. A program that refused the leaf is passed through on its next
// connection, and a different program talking to the same host is still read. Before,
// one refusal hid the host from every program until a window passed.
func TestBridgeProgram_ARefusingProgramIsPassedThroughAndOthersStillBridged(t *testing.T) {
	procs := newFakeProcs(fproc(300, 1, exeHelm), fproc(400, 1, exeCurl))
	store := session.New(0, 0, 0)
	defer store.Close()
	sc := newBridgeScene(t, store, procs)
	helm := tlsbridge.Program{Exe: exeHelm}.Key()

	if _, err := sc.handshake(t, procs, 300, sc.refusing); err == nil {
		t.Fatal("first connection: helm completed a handshake on a leaf it does not trust")
	}
	eventually(t, func() bool { return sc.engine.Programs.Contains(helm) }, "helm's refusal to be recorded against helm")
	if sc.engine.Skip.Contains(hostOnly(sc.target)) {
		t.Error("helm's refusal was recorded against the host too, which hides the host from every program")
	}

	bridged, err := sc.handshake(t, procs, 300, sc.refusing)
	if err != nil {
		t.Fatalf("second connection: helm was not passed through: %v", err)
	}
	if bridged {
		t.Fatal("second connection: helm was shown the bridge's leaf again")
	}
	eventually(t, func() bool {
		return slices.Contains(tunnelReasons(store, session.DefaultSessionID), pipeline.TunnelProgramRefused)
	}, "a program-refused tunnel row")

	if bridged, err := sc.handshake(t, procs, 400, sc.trusting); err != nil || !bridged {
		t.Fatalf("curl trusts the CA but was not bridged to the same host (bridged=%v, err=%v)", bridged, err)
	}
}

// Review focus 3. The same interpreter run by an agent and run on its own can disagree
// about the CA, so one refusing must not pass the other through.
func TestBridgeProgram_SameExecutableUnderAnAgentIsTrackedApart(t *testing.T) {
	procs := newFakeProcs(fproc(100, 1, exeClaude), fproc(500, 100, exePython), fproc(600, 1, exePython))
	store := session.New(0, 0, 0)
	defer store.Close()
	sc := newBridgeScene(t, store, procs)
	// Naming a session through its own header is what makes pid 100 an agent.
	sendAs(t, procs.clientFor(sc.proxyURL, 100), sc.backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")

	if _, err := sc.handshake(t, procs, 600, sc.refusing); err == nil {
		t.Fatal("python3 on its own completed a handshake on a leaf it does not trust")
	}
	eventually(t, func() bool { return sc.engine.Programs.Contains(tlsbridge.Program{Exe: exePython}.Key()) },
		"python3's refusal to be recorded with no agent")
	if bridged, err := sc.handshake(t, procs, 500, sc.trusting); err != nil || !bridged {
		t.Fatalf("python3 under the agent was passed through because python3 on its own refused (bridged=%v, err=%v)", bridged, err)
	}
}

// A client whose program cannot be named keeps today's behaviour: its refusal is
// recorded against the host, and the host memory decides its next connection. A named
// program ignores the host memory, and its success clears the host's entry.
func TestBridgeProgram_AnUnnamedClientKeepsTheHostMemory(t *testing.T) {
	procs := newFakeProcs(fproc(400, 1, exeCurl)) // pid 999 is in no table, so its executable is unknown
	store := session.New(0, 0, 0)
	defer store.Close()
	sc := newBridgeScene(t, store, procs)
	host := hostOnly(sc.target)

	if _, err := sc.handshake(t, procs, 999, sc.refusing); err == nil {
		t.Fatal("the unnamed client completed a handshake on a leaf it does not trust")
	}
	eventually(t, func() bool { return sc.engine.Skip.Contains(host) }, "the refusal to be recorded against the host")
	if bridged, err := sc.handshake(t, procs, 999, sc.refusing); err != nil || bridged {
		t.Fatalf("the unnamed client's retry was not tunnelled by the host memory (bridged=%v, err=%v)", bridged, err)
	}
	if bridged, err := sc.handshake(t, procs, 400, sc.trusting); err != nil || !bridged {
		t.Fatalf("curl was held back by another client's host entry (bridged=%v, err=%v)", bridged, err)
	}
	eventually(t, func() bool { return !sc.engine.Skip.Contains(host) }, "curl's success to clear the host's entry")
}

func TestBridgeServe_RecordsANamedProgramsRefusalAgainstTheProgram(t *testing.T) {
	s, _, authority := bridgeForRejectTest(t)
	s.TLSBridge.Programs = tlsbridge.NewProgramSkipSet()
	host := hostOnly(authority)
	prog := tlsbridge.Program{Exe: exeHelm}
	tl := discardTunnel()
	tl.program = &prog

	s.bridgeServe(rejectingClient(t), authority, host, tl)
	if !s.TLSBridge.Programs.Contains(prog.Key()) {
		t.Error("the refusal was not recorded against the program")
	}
	if s.TLSBridge.Skip.Contains(host) {
		t.Error("the refusal was recorded against the host as well")
	}
}

// Review focus 1: a tunnel with no program on it — the transparent listener, and every
// caller handing bridgeServe a bare tunnelLog — records against the host, as before.
func TestBridgeServe_UnnamedTunnelRecordsAgainstTheHost(t *testing.T) {
	s, _, authority := bridgeForRejectTest(t)
	s.TLSBridge.Programs = tlsbridge.NewProgramSkipSet()
	host := hostOnly(authority)

	s.bridgeServe(rejectingClient(t), authority, host, discardTunnel())
	if !s.TLSBridge.Skip.Contains(host) {
		t.Error("an unnamed tunnel's refusal was not recorded against the host")
	}
}

// Review focus 2: an Engine without program memory (cortex-envoy, cortex-cpex) records
// against the host even when a program was named, and does not panic.
func TestBridgeServe_ProgramWithoutProgramMemoryFallsBackToTheHost(t *testing.T) {
	s, _, authority := bridgeForRejectTest(t) // no Programs
	host := hostOnly(authority)
	tl := discardTunnel()
	tl.program = &tlsbridge.Program{Exe: exeHelm}

	s.bridgeServe(rejectingClient(t), authority, host, tl)
	if !s.TLSBridge.Skip.Contains(host) {
		t.Error("with no program memory the refusal was not recorded against the host")
	}
}

func TestBridgeServe_ASuccessClearsTheProgramsEntry(t *testing.T) {
	s, _, authority := bridgeForRejectTest(t)
	s.TLSBridge.Programs = tlsbridge.NewProgramSkipSet()
	host := hostOnly(authority)
	prog := tlsbridge.Program{Exe: exeCurl}
	s.TLSBridge.Programs.Fail(prog.Key())
	tl := discardTunnel()
	tl.program = &prog

	// bridgeServe blocks serving the decrypted connection, so run it and wait.
	go s.bridgeServe(trustingClient(t, s.TLSBridge.CAPEM), authority, host, tl)
	eventually(t, func() bool { return !s.TLSBridge.Programs.Contains(prog.Key()) },
		"a completed handshake to clear the program's entry")
}

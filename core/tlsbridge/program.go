package tlsbridge

// Program names the client program behind a bridged connection, for the per-program
// skip set (NewProgramSkipSet). Exe is the client process's executable, absolute and
// symlink-resolved. Agent is the executable of the nearest process in its ancestry
// that has named a session, and empty when none has.
//
// The pair is the identity, not Exe alone. Whether a program trusts the bridge CA
// depends on what it was told, and an agent's children inherit the CA variables the
// agent's settings export while the same interpreter run by cron does not — so
// python3 under Claude Code and python3 on its own can disagree, and one memory for
// both would flip between them the way a host-keyed one flips between clients.
type Program struct {
	Exe   string
	Agent string
}

// Key is p as a SkipSet key. NUL separates the halves because no path contains one.
func (p Program) Key() string { return p.Exe + "\x00" + p.Agent }

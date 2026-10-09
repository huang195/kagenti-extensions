package tlsbridge

import "testing"

// The agent half is the point of the key: the same interpreter run by an agent
// (which exports the CA variables) and by cron (which does not) can disagree about
// trusting the CA, and must not share one memory.
func TestProgramKey_SeparatesTheSameExecutableUnderDifferentAgents(t *testing.T) {
	underAgent := Program{Exe: "/usr/bin/python3", Agent: "/bin/claude"}.Key()
	alone := Program{Exe: "/usr/bin/python3"}.Key()
	if underAgent == alone {
		t.Fatalf("both keys are %q; python3 under an agent and python3 alone must be remembered apart", alone)
	}
	if (Program{Exe: "/a", Agent: "/b"}).Key() == (Program{Exe: "/a/b"}).Key() {
		t.Fatal("the key is ambiguous: a separator that can appear in a path joins two programs into one")
	}
}

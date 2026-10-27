package alloctest

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

var sink []byte

func TestZeroPassesAFunctionThatDoesNotAllocate(t *testing.T) {
	buf := make([]byte, 64)
	Zero(t, "a copy into a buffer", func() { copy(buf, "hello") })
}

// Zero fails a test whose function allocates, and skips it under the race
// detector. The failing test runs in a child process so this one can read
// its result.
func TestZeroFailsAFunctionThatAllocates(t *testing.T) {
	if os.Getenv("ALLOCTEST_CHILD") == "1" {
		Zero(t, "a new buffer", func() { sink = make([]byte, 64) })
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestZeroFailsAFunctionThatAllocates$", "-test.v")
	cmd.Env = append(os.Environ(), "ALLOCTEST_CHILD=1")
	out, err := cmd.CombinedOutput()
	if race {
		if err != nil || !strings.Contains(string(out), "--- SKIP") {
			t.Errorf("under the race detector the child should skip; err %v, output:\n%s", err, out)
		}
		return
	}
	if err == nil || !strings.Contains(string(out), "a new buffer allocates 1.0 times per call") {
		t.Errorf("the child should fail naming the call; err %v, output:\n%s", err, out)
	}
}

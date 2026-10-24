package chorefile

import (
	"strings"
	"testing"
	"time"
)

func TestCacheIsADuration(t *testing.T) {
	f, err := Decode([]byte(`
tasks:
  cached:
    cache: 5m
    cmds: ['true']
  plain:
    cmds: ['true']
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(f.Tasks["cached"].Cache); got != 5*time.Minute {
		t.Errorf("cache: 5m decoded as %v", got)
	}
	// Zero is "no cache": a task that does not ask for one is never replayed.
	if got := f.Tasks["plain"].Cache; got != 0 {
		t.Errorf("a task with no cache: got %v", time.Duration(got))
	}
}

// A lifetime with no unit is refused, as `timeout:` refuses one: a cache that
// lived sixty times longer than meant because the unit was assumed would replay
// an answer long after it stopped being true.
func TestCacheNeedsAUnitAndATime(t *testing.T) {
	for _, value := range []string{"5", "0s"} {
		_, err := Decode([]byte("tasks:\n  t:\n    cache: " + value + "\n    cmds: ['true']\n"))
		if err == nil {
			t.Errorf("cache: %s was accepted", value)
		}
	}
}

// What cannot be replayed is refused where it is written, not discovered on the
// second run.
func TestCacheRefusesWhatCannotBeReplayed(t *testing.T) {
	for _, c := range []struct {
		name, body, want string
	}{
		{
			name: "interactive",
			body: "    interactive: true\n",
			want: "interactive",
		},
		{
			name: "confirm",
			body: "    confirm: 'Deploy?'\n",
			want: "confirm",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Decode([]byte("tasks:\n  t:\n    cache: 5m\n" + c.body + "    cmds: ['true']\n"))
			if err == nil {
				t.Fatalf("cache: with %s was accepted", c.name)
			}
			if !strings.Contains(err.Error(), "cache") || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q; want it to name cache and %s", err, c.want)
			}
		})
	}
}

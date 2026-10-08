package storymap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/storymap"
)

const work = "testdata/work"

func TestLoadReportsProblemsWithoutFailing(t *testing.T) {
	l, err := storymap.Load(work, "arcade")
	if err != nil {
		t.Fatal(err)
	}
	if l.Map.Title != "Arcade" || len(l.Map.Journey) != 2 || len(l.Tasks) != 5 {
		t.Fatalf("loaded %+v", l)
	}
	if len(l.Problems) != 1 || !strings.Contains(l.Problems[0], `task broken: unknown activity "nowhere"`) {
		t.Fatalf("problems: %v", l.Problems)
	}
	list, err := storymap.List(work)
	if err != nil || len(list) != 1 || list[0].Tasks != 5 || list[0].Done != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
}

func TestSelectScopes(t *testing.T) {
	l, _ := storymap.Load(work, "arcade")
	cases := []struct {
		kind, id, release string
		want              string
	}{
		{"task", "pause", "", "pause"},
		{"activity", "play-game", "", "restart,pause"},
		{"phase", "play", "", "restart,pause,leaderboard"},
		{"phase", "play", "next", "leaderboard"},
		// done tasks are left out of bigger scopes
		{"phase", "discover", "", ""},
	}
	for _, c := range cases {
		sc, _, err := l.Select(c.kind, c.id, c.release)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s %s: want an error for an empty scope", c.kind, c.id)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s %s: %v", c.kind, c.id, err)
		}
		if got := strings.Join(sc.Tasks, ","); got != c.want {
			t.Errorf("%s %s %s: got %s, want %s", c.kind, c.id, c.release, got, c.want)
		}
	}
	if _, _, err := l.Select("phase", "play", "nope"); err == nil {
		t.Error("unknown release must fail")
	}
}

func TestBriefCarriesTheStoryAndContext(t *testing.T) {
	l, _ := storymap.Load(work, "arcade")
	sc, tasks, err := l.Select("task", "home-grid", "")
	if err != nil {
		t.Fatal(err)
	}
	brief := l.Brief(sc, tasks)
	for _, want := range []string{
		"user story map **Arcade**", "product/user-story-maps/arcade/",
		"Journey: Discover → Browse games", "Persona: Casual player", "Release: MVP (goal: one great game loop)",
		"Story: As a player I want", "- the grid works on a phone", "Should move metric: Weekly players (target 100)",
		"Do not edit files under `product/`",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
	if got := l.Title(sc, tasks); got != "See every game on the home page" {
		t.Errorf("title %q", got)
	}
	sc, tasks, _ = l.Select("activity", "play-game", "")
	if got := l.Title(sc, tasks); got != "Arcade: Play a game (2 user tasks)" {
		t.Errorf("title %q", got)
	}
}

func TestChecks(t *testing.T) {
	l, _ := storymap.Load(work, "arcade")
	bad := l.Map
	bad.Metrics = append(bad.Metrics, storymap.Metric{ID: "x", Title: "x", Kind: "delivery", Measure: "vibes"})
	bad.Releases = append(bad.Releases, storymap.Release{ID: "mvp", Title: "dup"})
	err := bad.Check()
	if err == nil || !strings.Contains(err.Error(), "measure must be one of") || !strings.Contains(err.Error(), `release id "mvp" is used twice`) {
		t.Fatalf("got %v", err)
	}
	task := storymap.Task{ID: "Bad ID", Title: "x", Activity: "browse"}
	if err := l.Map.CheckTask(&task); err == nil {
		t.Fatal("a non-slug id must fail")
	}
	task = storymap.Task{ID: "ok", Title: "x", Activity: "browse", Status: "shipped"}
	if err := l.Map.CheckTask(&task); err == nil || !strings.Contains(err.Error(), "status must be one of") {
		t.Fatalf("got %v", err)
	}
}

func TestMarshalKeepsFieldOrder(t *testing.T) {
	b, err := storymap.Marshal(storymap.Task{ID: "x", Title: "T", Activity: "a", Release: "r", Status: "todo"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "title: T\nactivity: a\nrelease: r\nstatus: todo\n" {
		t.Fatalf("got %q", b)
	}
	// A written map reads back the same.
	l, _ := storymap.Load(work, "arcade")
	dir := t.TempDir()
	out, _ := storymap.Marshal(l.Map)
	p := filepath.Join(dir, filepath.FromSlash(storymap.MapPath("copy")))
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, out, 0o644)
	back, err := storymap.Load(dir, "copy")
	if err != nil || back.Map.Title != "Arcade" || *back.Map.Metrics[0].Target != 100 {
		t.Fatalf("%v %+v", err, back)
	}
}

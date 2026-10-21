package workspace

import "testing"

// fakeResolver answers from maps, so the rule can be exercised without
// building real repositories on disk. A directory absent from tops behaves
// exactly as a deleted worktree does: not a repository.
type fakeResolver struct {
	tops map[string]string
	urls map[string]string
}

func (f fakeResolver) Toplevel(dir string) (string, bool) {
	top, ok := f.tops[dir]
	return top, ok
}

func (f fakeResolver) OriginURL(top string) (string, bool) {
	url, ok := f.urls[top]
	return url, ok
}

func TestRepoName(t *testing.T) {
	cases := []struct {
		url, want string
	}{
		{"git@github.com:acme/api.git", "acme/api"},
		{"git@github.com:acme/api", "acme/api"},
		{"https://github.com/acme/agents.git", "acme/agents"},
		{"https://github.com/acme/agents", "acme/agents"},
		{"ssh://git@github.com/acme/api-v2.git", "acme/api-v2"},
		{"https://user:token@github.com/acme/api.git", "acme/api"},
		{"git@github.com:acme/api.git/", "acme/api"},
		// A nested group path keeps every segment: truncating to the last two
		// would merge two distinct repositories that share a leaf name.
		{"https://gitlab.com/group/sub/repo.git", "group/sub/repo"},
		// Local paths have no host to strip, so they report "" and let the
		// caller fall back to the repository's own directory name.
		{"/Users/alice/repos/thing", ""},
		{"file:///Users/alice/repos/thing", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := RepoName(c.url); got != c.want {
			t.Errorf("RepoName(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestTailSegments(t *testing.T) {
	cases := []struct {
		path string
		n    int
		want string
	}{
		{"/Users/alice/Development/GitHub/acme", 2, "GitHub/acme"},
		{"/Users/alice/Development/GitHub/acme/api", 1, "api"},
		{"/thing", 2, "thing"},
		{"/Users/alice/proj/", 2, "alice/proj"},
		{"/", 2, "/"},
	}
	for _, c := range cases {
		if got := TailSegments(c.path, c.n); got != c.want {
			t.Errorf("TailSegments(%q, %d) = %q, want %q", c.path, c.n, got, c.want)
		}
	}
}

func TestNameUsesOriginRemote(t *testing.T) {
	r := fakeResolver{
		tops: map[string]string{"/w/acme/api": "/w/acme/api"},
		urls: map[string]string{"/w/acme/api": "git@github.com:acme/api.git"},
	}
	if got := Name([]string{"/w/acme/api"}, r); got != "acme/api" {
		t.Fatalf("Name = %q, want acme/api", got)
	}
}

// A directory inside a repository names the repository, not itself: work done
// in a subdirectory still belongs to the repo that contains it.
func TestNameResolvesSubdirectoryToRepo(t *testing.T) {
	r := fakeResolver{
		tops: map[string]string{"/w/acme/api/internal/clearing": "/w/acme/api"},
		urls: map[string]string{"/w/acme/api": "git@github.com:acme/api.git"},
	}
	if got := Name([]string{"/w/acme/api/internal/clearing"}, r); got != "acme/api" {
		t.Fatalf("Name = %q, want acme/api", got)
	}
}

// The whole reason Name walks the ranked list: a session's busiest directory
// is routinely a worktree that has since been deleted, and the repository it
// came from is further down.
func TestNameSkipsUnresolvableDirectories(t *testing.T) {
	r := fakeResolver{
		tops: map[string]string{"/w/acme/api": "/w/acme/api"},
		urls: map[string]string{"/w/acme/api": "git@github.com:acme/api.git"},
	}
	cwds := []string{"/w/acme/.worktrees/eng-19081", "/w/acme/.worktrees/eng-19083", "/w/acme/api"}
	if got := Name(cwds, r); got != "acme/api" {
		t.Fatalf("Name = %q, want acme/api", got)
	}
}

// A repository with no origin remote (this repo is one) takes its toplevel's
// directory name rather than falling through to the path fallback.
func TestNameFallsBackToToplevelName(t *testing.T) {
	r := fakeResolver{
		tops: map[string]string{"/w/local/thing/sub": "/w/local/thing"},
		urls: map[string]string{},
	}
	if got := Name([]string{"/w/local/thing/sub"}, r); got != "thing" {
		t.Fatalf("Name = %q, want thing", got)
	}
}

// A remote git cannot reduce to owner/repo is treated the same as no remote.
func TestNameFallsBackWhenRemoteIsLocalPath(t *testing.T) {
	r := fakeResolver{
		tops: map[string]string{"/w/local/thing": "/w/local/thing"},
		urls: map[string]string{"/w/local/thing": "/Users/alice/mirrors/thing"},
	}
	if got := Name([]string{"/w/local/thing"}, r); got != "thing" {
		t.Fatalf("Name = %q, want thing", got)
	}
}

// No directory resolves at all: the workspace root case, where the session ran
// in a plain folder holding several repositories.
func TestNameFallsBackToPath(t *testing.T) {
	r := fakeResolver{tops: map[string]string{}, urls: map[string]string{}}
	cwds := []string{"/Users/alice/Development/GitHub/acme", "/Users/alice/Development/GitHub/acme/.project/x"}
	if got := Name(cwds, r); got != "GitHub/acme" {
		t.Fatalf("Name = %q, want GitHub/acme", got)
	}
}

// An empty list is the one case Name declines to answer, so callers can tell
// "no working directory recorded" apart from a name it derived.
func TestNameEmpty(t *testing.T) {
	r := fakeResolver{tops: map[string]string{}, urls: map[string]string{}}
	if got := Name(nil, r); got != "" {
		t.Fatalf("Name = %q, want empty", got)
	}
}

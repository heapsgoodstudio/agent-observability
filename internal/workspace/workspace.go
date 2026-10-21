// Package workspace turns a session's ranked working directories into the
// descriptive name the dashboard shows in place of Claude Code's own project
// directory key.
//
// The key it replaces is the encoded launch path
// ("-Users-alice-Development-GitHub-acme"), which says where a session was
// started rather than what it worked on. Sessions launched from a workspace
// root that holds several repos all collapse onto one such key even though
// each spent its records inside a different repo, so the launch directory is
// the least useful directory a session mentions.
//
// The rule, in order:
//
//  1. Walk the ranked directories (most-mentioned first) and stop at the
//     first one that still resolves to a git repository.
//  2. Name it "owner/repo" from that repository's origin remote.
//  3. A repository with no origin remote takes its toplevel's directory name.
//  4. If no directory resolves at all, take the last two segments of the
//     most-mentioned one.
//
// Step 1 walking rather than taking only the top entry is what handles
// worktrees: throwaway checkouts under .worktrees/ are routinely deleted, and
// a session whose busiest directory is now gone still resolves through its
// next-busiest, which is usually the repository the worktree came from.
package workspace

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// Resolver answers the two git questions Name needs. It is an interface so
// tests can exercise the rule without building real repositories on disk;
// NewGitResolver is the only implementation that shells out.
type Resolver interface {
	// Toplevel reports dir's git repository root, or false when dir is not in
	// a repository (including when dir no longer exists).
	Toplevel(dir string) (string, bool)
	// OriginURL reports the "origin" remote's URL for the repository rooted
	// at top, or false when it has no such remote.
	OriginURL(top string) (string, bool)
}

// Name applies the package rule to cwds, which must be ranked most-mentioned
// first (transcript.Summary.Cwds is already in that order). It returns "" only
// when cwds is empty, so callers keep their own fallback for that one case.
func Name(cwds []string, r Resolver) string {
	for _, cwd := range cwds {
		top, ok := r.Toplevel(cwd)
		if !ok {
			continue
		}
		if url, ok := r.OriginURL(top); ok {
			if name := RepoName(url); name != "" {
				return name
			}
		}
		return filepath.Base(top)
	}
	if len(cwds) == 0 {
		return ""
	}
	return TailSegments(cwds[0], 2)
}

// RepoName reduces a git remote URL to its "owner/repo" path, dropping the
// scheme, any user@ prefix, the host, and a trailing ".git". It handles both
// URL forms git uses ("https://github.com/owner/repo.git" and the scp-like
// "git@github.com:owner/repo.git") and returns everything after the host, so a
// nested group path survives intact rather than being silently truncated to
// its last two parts.
//
// It returns "" for anything with no host to strip, which is the signal that
// the remote is a local filesystem path and the caller should fall back to the
// repository's own directory name instead.
func RepoName(url string) string {
	s := strings.TrimSpace(url)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	// Anything up to the last "@" of the authority is credentials, not
	// content. The authority ends at the first "/", and the last "@" within it
	// is the separator: "user:token@host" carries a colon of its own, so
	// searching the whole string for the first ":" would mistake the password
	// separator for the scp form's host separator.
	authority := s
	if slash := strings.IndexByte(s, '/'); slash >= 0 {
		authority = s[:slash]
	}
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		s = s[at+1:]
	}
	// A bare or post-scheme absolute path has no host to drop; the split
	// below would mistake its leading "" for one.
	if strings.HasPrefix(s, "/") {
		return ""
	}
	// In the scp-like form the host is separated by ":" rather than "/".
	s = strings.Replace(s, ":", "/", 1)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")

	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return ""
	}
	name := strings.Join(parts[1:], "/")
	if name == "" {
		return ""
	}
	return name
}

// TailSegments returns path's last n segments joined by "/", or the whole
// cleaned path when it has fewer. A path that reduces to nothing (root, or
// empty) comes back as the cleaned path itself rather than "".
func TailSegments(path string, n int) string {
	clean := filepath.Clean(path)
	parts := strings.Split(strings.Trim(clean, string(filepath.Separator)), string(filepath.Separator))
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return clean
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return strings.Join(kept, "/")
}

/* git resolver -------------------------------------------------------------- */

// gitResolver answers Toplevel and OriginURL by running git, memoising both
// answers per directory. The memo is the point: an indexing pass re-parses
// hundreds of sessions that share a few dozen directories between them, so
// without it the same two commands run once per session rather than once per
// directory.
type gitResolver struct {
	git  string
	tops map[string]topResult
	urls map[string]urlResult
}

type topResult struct {
	top string
	ok  bool
}

type urlResult struct {
	url string
	ok  bool
}

// NewGitResolver returns a Resolver backed by the git binary. When no git can
// be found it returns a resolver that answers false to everything, which
// degrades Name to its path fallback rather than failing an indexing pass:
// a dashboard label is not worth breaking indexing over.
//
// The returned value is not safe for concurrent use.
func NewGitResolver() Resolver {
	git, err := exec.LookPath("git")
	if err != nil {
		// Cleared explicitly: LookPath returns a non-empty path alongside its
		// error for some inputs, and "" is this type's only signal for "no git".
		git = ""
		// Launchd agents start with a minimal PATH that need not include the
		// Xcode command line tools' shim directory, so an absent git here is
		// routine rather than a sign that git is missing from the machine.
		if _, statErr := exec.LookPath("/usr/bin/git"); statErr == nil {
			git = "/usr/bin/git"
		}
	}
	return &gitResolver{
		git:  git,
		tops: map[string]topResult{},
		urls: map[string]urlResult{},
	}
}

func (g *gitResolver) Toplevel(dir string) (string, bool) {
	if g.git == "" || dir == "" {
		return "", false
	}
	if r, ok := g.tops[dir]; ok {
		return r.top, r.ok
	}
	out, err := g.run(dir, "rev-parse", "--show-toplevel")
	r := topResult{top: out, ok: err == nil && out != ""}
	g.tops[dir] = r
	return r.top, r.ok
}

func (g *gitResolver) OriginURL(top string) (string, bool) {
	if g.git == "" || top == "" {
		return "", false
	}
	if r, ok := g.urls[top]; ok {
		return r.url, r.ok
	}
	out, err := g.run(top, "remote", "get-url", "origin")
	r := urlResult{url: out, ok: err == nil && out != ""}
	g.urls[top] = r
	return r.url, r.ok
}

// run executes one git subcommand against dir and returns its trimmed stdout.
// Stderr is discarded: every failure this package cares about (not a
// repository, directory gone, no such remote) is reported by the exit status,
// and the message behind it has nowhere useful to go.
func (g *gitResolver) run(dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	out, err := exec.Command(g.git, full...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

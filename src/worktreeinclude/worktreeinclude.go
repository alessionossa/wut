// Package worktreeinclude copies untracked files listed in a repo's
// .worktreeinclude file into a freshly created worktree.
//
// Only files that are BOTH matched by a .worktreeinclude pattern AND 
// gitignored in the source repo are copied, so tracked files are never 
// duplicated. Errors are swallowed and surface as warnings on 
// stderr — a misconfigured .worktreeinclude must not break `wut new`.
package worktreeinclude

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	dotignore "github.com/codeglyph/go-dotignore/v2"
	"github.com/simonbs/wut/src/git"
)

// Copy reads <repoRoot>/.worktreeinclude, finds gitignored files matching its
// patterns, and copies them into worktreePath. Returns the relative paths of
// the files that were copied. Missing or empty .worktreeinclude is a no-op.
// Structural errors (read failure, git failure, matcher build failure) cause
// an early return with no copies; per-file copy failures are logged to stderr
// and do not abort the rest of the copies.
func Copy(repoRoot, worktreePath string) []string {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".worktreeinclude"))
	if err != nil {
		return nil
	}

	patterns := parsePatterns(string(data))
	if len(patterns) == 0 {
		return nil
	}

	matcher, err := dotignore.NewPatternMatcher(patterns)
	if err != nil {
		return nil
	}

	// --directory collapses fully-gitignored dirs (node_modules/, .turbo/, ...)
	// into single trailing-slash entries. In a large repo this avoids walking
	// hundreds of thousands of files just to filter them out.
	out, err := git.Run(
		[]string{"ls-files", "--others", "--ignored", "--exclude-standard", "--directory"},
		repoRoot,
	)
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}

	collapsedDirs, files := splitEntries(out, matcher)

	// A .worktreeinclude pattern may target a path inside a collapsed dir
	// (e.g. `config/secrets/api.key` when all of `config/secrets/` is
	// gitignored). For those, expand only the dirs the pattern actually
	// reaches into — expanding every collapsed dir would defeat the perf win.
	if expand := dirsToExpand(collapsedDirs, patterns, matcher); len(expand) > 0 {
		args := append(
			[]string{"ls-files", "--others", "--ignored", "--exclude-standard", "--"},
			expand...,
		)
		expanded, err := git.Run(args, repoRoot)
		if err == nil {
			for _, f := range strings.Split(strings.TrimSpace(expanded), "\n") {
				if f == "" {
					continue
				}
				if ok, _ := matcher.Matches(f); ok {
					files = append(files, f)
				}
			}
		}
	}

	copied := make([]string, 0, len(files))
	for _, rel := range files {
		src := filepath.Join(repoRoot, rel)
		dst := filepath.Join(worktreePath, rel)
		if err := copyFile(src, dst); err != nil {
			fmt.Fprintf(os.Stderr, "wut: failed to copy %s to worktree: %v\n", rel, err)
			continue
		}
		copied = append(copied, rel)
	}

	if len(copied) > 0 {
		fmt.Fprintf(
			os.Stderr,
			"wut: copied %d file(s) from .worktreeinclude: %s\n",
			len(copied),
			strings.Join(copied, ", "),
		)
	}
	return copied
}

func parsePatterns(content string) []string {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func splitEntries(out string, matcher *dotignore.PatternMatcher) (dirs, files []string) {
	for _, e := range strings.Split(strings.TrimSpace(out), "\n") {
		if e == "" {
			continue
		}
		if strings.HasSuffix(e, "/") {
			dirs = append(dirs, e)
			continue
		}
		if ok, _ := matcher.Matches(e); ok {
			files = append(files, e)
		}
	}
	return dirs, files
}

// dirsToExpand returns the collapsed dirs that .worktreeinclude actually
// reaches into. We expand when a pattern's literal prefix lands inside the
// dir, or when the dir itself matches the include set. We deliberately skip
// anchorless patterns like `**/foo` or `*.tmp` — for those, expanding every
// collapsed dir would scan the whole gitignored tree, which is the cost
// `--directory` exists to avoid.
func dirsToExpand(dirs, patterns []string, matcher *dotignore.PatternMatcher) []string {
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if patternsTargetDir(dir, patterns) {
			out = append(out, dir)
			continue
		}
		if ok, _ := matcher.Matches(strings.TrimSuffix(dir, "/")); ok {
			out = append(out, dir)
		}
	}
	return out
}

func patternsTargetDir(dir string, patterns []string) bool {
	for _, p := range patterns {
		normalized := strings.TrimPrefix(p, "/")
		if strings.HasPrefix(normalized, dir) {
			return true
		}
		if globIdx := strings.IndexAny(normalized, "*?["); globIdx > 0 {
			if strings.HasPrefix(dir, normalized[:globIdx]) {
				return true
			}
		}
	}
	return false
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

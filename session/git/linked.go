package git

import (
	"bufio"
	"claude-squad/config"
	"claude-squad/log"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// LinkedReposFile lists other repos that every session should also get a worktree of.
// It lives at the root of the repo cs is launched from. One repo per line:
//
//	<alias> <path>
//
// The path may be absolute or relative to the repo root. Lines starting with # are ignored.
const LinkedReposFile = ".claude-squad-repos"

// LinkedState is the serializable state of one linked worktree.
type LinkedState struct {
	Alias            string `json:"alias"`
	RepoPath         string `json:"repo_path"`
	WorktreePath     string `json:"worktree_path"`
	BaseCommitSHA    string `json:"base_commit_sha"`
	IsExistingBranch bool   `json:"is_existing_branch"`
}

type linkedRepo struct{ alias, path string }

// readLinkedRepos parses LinkedReposFile in repoRoot; no file means no linked repos.
func readLinkedRepos(repoRoot string) ([]linkedRepo, error) {
	f, err := os.Open(filepath.Join(repoRoot, LinkedReposFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", LinkedReposFile, err)
	}
	defer f.Close()

	var repos []linkedRepo
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid line in %s: %q (want '<alias> <path>')", LinkedReposFile, line)
		}
		path := fields[1]
		if !filepath.IsAbs(path) {
			path = filepath.Join(repoRoot, path)
		}
		root, err := findGitRepoRoot(path)
		if err != nil {
			return nil, fmt.Errorf("linked repo %s: %w", fields[0], err)
		}
		repos = append(repos, linkedRepo{alias: fields[0], path: root})
	}
	return repos, scanner.Err()
}

// attachLinked registers (but does not set up) a worktree per repo in LinkedReposFile.
func (g *GitWorktree) attachLinked() error {
	repos, err := readLinkedRepos(g.repoPath)
	if err != nil {
		return err
	}
	if len(repos) > 0 {
		if trashRoot, err := trashDir(); err == nil {
			go emptyTrash(trashRoot) // leftovers from a cs that quit mid-delete
		}
	}
	for _, r := range repos {
		g.linked = append(g.linked, &GitWorktree{
			repoPath:     r.path,
			worktreePath: g.worktreePath + "." + r.alias,
			sessionName:  g.sessionName,
			branchName:   g.branchName,
			alias:        r.alias,
		})
	}
	return nil
}

// SearchBranchesAll is SearchBranches over the launch repo and its linked repos, so an
// existing api or web branch can be picked even if the launch repo has never seen it.
func SearchBranchesAll(dir, filter string) ([]string, error) {
	root, err := findGitRepoRoot(dir)
	if err != nil {
		return nil, err
	}
	repos, err := readLinkedRepos(root)
	if err != nil || len(repos) == 0 {
		return SearchBranches(root, filter)
	}
	seen := make(map[string]bool)
	var all []string
	for _, path := range append([]string{root}, linkedPaths(repos)...) {
		branches, err := SearchBranches(path, filter)
		if err != nil {
			return nil, err
		}
		for _, b := range branches {
			if !seen[b] && len(all) < MaxBranchSearchResults {
				seen[b] = true
				all = append(all, b)
			}
		}
	}
	return all, nil
}

func linkedPaths(repos []linkedRepo) []string {
	var paths []string
	for _, r := range repos {
		paths = append(paths, r.path)
	}
	return paths
}

// LinkedStates returns the serializable state of the linked worktrees.
func (g *GitWorktree) LinkedStates() []LinkedState {
	var states []LinkedState
	for _, l := range g.linked {
		states = append(states, LinkedState{
			Alias:            l.alias,
			RepoPath:         l.repoPath,
			WorktreePath:     l.worktreePath,
			BaseCommitSHA:    l.baseCommitSHA,
			IsExistingBranch: l.isExistingBranch,
		})
	}
	return states
}

// Slot returns the session's port slot (0 if none).
func (g *GitWorktree) Slot() int { return g.slot }

// SetSlot restores the port slot from storage.
func (g *GitWorktree) SetSlot(slot int) { g.slot = slot }

// SetLinkedStates restores linked worktrees from storage.
func (g *GitWorktree) SetLinkedStates(states []LinkedState) {
	g.linked = nil
	for _, s := range states {
		g.linked = append(g.linked, &GitWorktree{
			repoPath:         s.RepoPath,
			worktreePath:     s.WorktreePath,
			sessionName:      g.sessionName,
			branchName:       g.branchName,
			baseCommitSHA:    s.BaseCommitSHA,
			isExistingBranch: s.IsExistingBranch,
			alias:            s.Alias,
		})
	}
}

// Setup creates the session worktree and one worktree per linked repo, each symlinked
// into the session worktree as ./<alias>.
func (g *GitWorktree) Setup() error {
	// With linked repos, Setup also runs on resume when only one side is missing,
	// so never recreate a worktree that is still intact (it would drop uncommitted work).
	if valid, _ := g.IsValidWorktreeSelf(); !valid || len(g.linked) == 0 {
		// A branch picked from a linked repo may not exist in the launch repo yet;
		// create it there (and own it, so kill removes it) instead of failing.
		if g.isExistingBranch && len(g.linked) > 0 && !g.branchExists() {
			g.isExistingBranch = false
		}
		if err := g.SetupSelf(); err != nil {
			return err
		}
	}
	for _, l := range g.linked {
		if err := l.setupLinked(); err != nil {
			return fmt.Errorf("linked repo %s: %w", l.alias, err)
		}
		link := filepath.Join(g.worktreePath, l.alias)
		_ = os.Remove(link)
		if err := os.Symlink(l.worktreePath, link); err != nil {
			return fmt.Errorf("failed to link %s into session: %w", l.alias, err)
		}
	}
	if len(g.linked) == 0 {
		return nil
	}
	if err := g.claimSlot(); err != nil {
		return err
	}
	g.writeSlotRecord()
	g.runHook("setup")
	return nil
}

// setupLinked creates a linked worktree on the session branch. New branches start
// from the repo's default branch (origin/HEAD), not whatever the main checkout is on.
func (l *GitWorktree) setupLinked() error {
	if valid, _ := l.IsValidWorktreeSelf(); valid {
		return nil
	}
	_, _ = l.runGitCommand(l.repoPath, "worktree", "remove", "-f", l.worktreePath)
	_ = os.RemoveAll(l.worktreePath)

	firstSetup := l.baseCommitSHA == ""
	if l.branchExists() {
		// A branch we didn't create is never deleted on kill.
		if firstSetup {
			l.isExistingBranch = true
		}
		base := l.baseCommitSHA
		if err := l.setupFromExistingBranch(); err != nil {
			return err
		}
		// Keep the original base across pause/resume so the diff still shows earlier work.
		if base != "" {
			l.baseCommitSHA = base
		}
		return nil
	}

	start := l.defaultRef()
	if remote, branch, ok := strings.Cut(start, "/"); ok && remote == "origin" {
		_, _ = l.runGitCommand(l.repoPath, "fetch", "origin", branch) // best-effort, may be offline
	}
	if _, err := l.runGitCommand(l.repoPath, "worktree", "add", "-b", l.branchName, l.worktreePath, start); err != nil {
		return fmt.Errorf("failed to create worktree from %s: %w", start, err)
	}
	return l.recordBaseCommit()
}

func (g *GitWorktree) branchExists() bool {
	_, localErr := g.runGitCommand(g.repoPath, "show-ref", "--verify", "refs/heads/"+g.branchName)
	_, remoteErr := g.runGitCommand(g.repoPath, "show-ref", "--verify", "refs/remotes/origin/"+g.branchName)
	return localErr == nil || remoteErr == nil
}

// defaultRef returns the remote default branch (e.g. origin/main), or HEAD if unknown.
func (l *GitWorktree) defaultRef() string {
	out, err := l.runGitCommand(l.repoPath, "rev-parse", "--abbrev-ref", "origin/HEAD")
	if err != nil {
		return "HEAD"
	}
	return strings.TrimSpace(out)
}

// Cleanup removes all worktrees and the branches this session created.
func (g *GitWorktree) Cleanup() error {
	var errs []error
	if len(g.linked) > 0 {
		g.runHook("teardown")
		g.releaseSlot()
	}
	for _, l := range g.linked {
		_ = os.Remove(filepath.Join(g.worktreePath, l.alias))
		if err := l.removeFast(); err != nil {
			errs = append(errs, fmt.Errorf("linked repo %s: %w", l.alias, err))
		}
		if err := l.CleanupSelf(); err != nil {
			errs = append(errs, fmt.Errorf("linked repo %s: %w", l.alias, err))
		}
	}
	if err := g.CleanupSelf(); err != nil {
		errs = append(errs, err)
	}
	if len(g.linked) > 0 {
		for _, hook := range []string{"setup", "pause", "teardown"} {
			_ = os.Remove(g.worktreePath + "." + hook + ".log")
		}
	}
	if len(errs) > 0 {
		return g.combineErrors(errs)
	}
	return nil
}

// Remove removes all worktrees but keeps their branches (pause). The pause hook runs first
// so it can set aside untracked state worth keeping, like dependency folders.
func (g *GitWorktree) Remove() error {
	if len(g.linked) > 0 {
		g.runHook("pause")
	}
	for _, l := range g.linked {
		_ = os.Remove(filepath.Join(g.worktreePath, l.alias))
		if err := l.removeFast(); err != nil {
			return fmt.Errorf("linked repo %s: %w", l.alias, err)
		}
	}
	return g.RemoveSelf()
}

// removeFast removes a linked worktree without blocking on deleting its files: linked
// worktrees hold big trees like node_modules, and `git worktree remove` deletes them inline
// (pause and kill froze the UI ~10s). The folder is moved to trash, git forgets it, and the
// files are deleted in the background. Callers commit first; uncommitted work is dropped,
// same as `git worktree remove -f`.
func (l *GitWorktree) removeFast() error {
	if _, err := os.Stat(l.worktreePath); os.IsNotExist(err) {
		return nil
	}
	trashRoot, err := trashDir()
	if err != nil {
		return l.RemoveSelf()
	}
	trash := filepath.Join(trashRoot, fmt.Sprintf("%s_%d", filepath.Base(l.worktreePath), time.Now().UnixNano()))
	if err := os.Rename(l.worktreePath, trash); err != nil {
		return l.RemoveSelf()
	}
	if err := l.PruneSelf(); err != nil {
		return err
	}
	go emptyTrash(trashRoot)
	return nil
}

func trashDir() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "trash")
	return dir, os.MkdirAll(dir, 0755)
}

// emptyTrash deletes everything in trash, including leftovers from a cs that exited mid-delete.
func emptyTrash(trashRoot string) {
	entries, _ := os.ReadDir(trashRoot)
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(trashRoot, e.Name()))
	}
}

// Prune prunes worktree metadata in every repo.
func (g *GitWorktree) Prune() error {
	for _, l := range g.linked {
		if err := l.PruneSelf(); err != nil {
			return fmt.Errorf("linked repo %s: %w", l.alias, err)
		}
	}
	return g.PruneSelf()
}

// IsValidWorktree reports whether every worktree of the session is intact.
func (g *GitWorktree) IsValidWorktree() (bool, error) {
	for _, l := range g.linked {
		if valid, err := l.IsValidWorktreeSelf(); err != nil || !valid {
			return valid, err
		}
	}
	return g.IsValidWorktreeSelf()
}

// IsBranchCheckedOut reports whether the session branch is checked out in any main checkout.
func (g *GitWorktree) IsBranchCheckedOut() (bool, error) {
	for _, l := range g.linked {
		if checked, err := l.IsBranchCheckedOutSelf(); err != nil || checked {
			return checked, err
		}
	}
	return g.IsBranchCheckedOutSelf()
}

// IsDirty reports whether any worktree of the session has uncommitted changes.
func (g *GitWorktree) IsDirty() (bool, error) {
	for _, l := range g.linked {
		if dirty, err := l.IsDirtySelf(); err != nil || dirty {
			return dirty, err
		}
	}
	return g.IsDirtySelf()
}

// CommitChanges commits uncommitted changes in every worktree.
func (g *GitWorktree) CommitChanges(commitMessage string) error {
	for _, l := range g.linked {
		if err := l.CommitChangesSelf(commitMessage); err != nil {
			return fmt.Errorf("linked repo %s: %w", l.alias, err)
		}
	}
	return g.CommitChangesSelf(commitMessage)
}

// PushChanges commits and pushes every worktree that has work on the branch. Linked repos
// with nothing beyond their default branch are skipped, as is a session repo with no remote.
func (g *GitWorktree) PushChanges(commitMessage string, open bool) error {
	if len(g.linked) == 0 {
		return g.PushChangesSelf(commitMessage, open)
	}
	if err := g.CommitChanges(commitMessage); err != nil {
		return err
	}
	pushed := 0
	for _, l := range g.linked {
		ok, err := l.pushLinked(open)
		if err != nil {
			return fmt.Errorf("linked repo %s: %w", l.alias, err)
		}
		if ok {
			pushed++
		}
	}
	if _, err := g.runGitCommand(g.worktreePath, "remote", "get-url", "origin"); err == nil {
		return g.PushChangesSelf(commitMessage, open)
	}
	if pushed == 0 {
		return fmt.Errorf("nothing to push: no repo has commits on %s", g.branchName)
	}
	return nil
}

// pushLinked pushes the branch if it has commits beyond the default branch.
func (l *GitWorktree) pushLinked(open bool) (bool, error) {
	out, err := l.runGitCommand(l.worktreePath, "rev-list", "--count", l.defaultRef()+"..HEAD")
	if err != nil {
		return false, err
	}
	if n, _ := strconv.Atoi(strings.TrimSpace(out)); n == 0 {
		return false, nil
	}
	cmd := exec.Command("git", "push", "-u", "origin", l.branchName)
	cmd.Dir = l.worktreePath
	if output, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("failed to push branch: %s (%w)", output, err)
	}
	if open {
		if err := l.OpenBranchURL(); err != nil {
			log.ErrorLog.Printf("failed to open branch URL: %v", err)
		}
	}
	return true, nil
}

// Diff returns one diff covering the session repo and every linked repo, with linked
// paths prefixed by their alias (e.g. api/squadhealth/models.py).
func (g *GitWorktree) Diff() *DiffStats {
	stats := g.DiffSelf()
	for _, l := range g.linked {
		ls := l.linkedDiff(false)
		mergeStats(stats, ls)
	}
	return stats
}

// DiffNumstat returns line counts across the session repo and every linked repo.
func (g *GitWorktree) DiffNumstat() *DiffStats {
	stats := g.DiffNumstatSelf()
	for _, l := range g.linked {
		mergeStats(stats, l.linkedDiff(true))
	}
	return stats
}

func (l *GitWorktree) linkedDiff(numstat bool) *DiffStats {
	stats := &DiffStats{}
	if _, err := l.runGitCommand(l.worktreePath, "add", "-N", "."); err != nil {
		stats.Error = err
		return stats
	}
	args := []string{"--no-pager", "diff",
		"--src-prefix=a/" + l.alias + "/", "--dst-prefix=b/" + l.alias + "/"}
	if numstat {
		args = append(args, "--numstat")
	}
	out, err := l.runGitCommand(l.worktreePath, append(args, l.baseCommitSHA)...)
	if err != nil {
		stats.Error = err
		return stats
	}
	if numstat {
		stats.Added, stats.Removed = parseNumstat(out)
		return stats
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			stats.Added++
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			stats.Removed++
		}
	}
	stats.Content = out
	return stats
}

func mergeStats(into, from *DiffStats) {
	into.Added += from.Added
	into.Removed += from.Removed
	into.Content += from.Content
	if into.Error == nil {
		into.Error = from.Error
	}
}

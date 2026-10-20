package git

import (
	"claude-squad/config"
	"claude-squad/log"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// HooksDir holds optional executable hooks in the repo cs is launched from:
//
//	setup     runs after every session worktree is created or recreated (new and resume)
//	pause     runs when a session is paused, before its worktrees are removed
//	teardown  runs when a session is killed, before its worktrees are removed
//
// Hooks get CS_SESSION, CS_BRANCH, CS_SLOT, CS_SESSION_DIR, and for each linked repo
// CS_REPO_<ALIAS> (its session worktree) and CS_MAIN_<ALIAS> (its main checkout).
// Output goes to <session dir>.<hook>.log. A failing hook is logged, never fatal.
const HooksDir = ".claude-squad"

const maxSlots = 99

func slotsDir() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "slots"), nil
}

// claimSlot gives the session the lowest free port slot, keeping any slot it already has.
func (g *GitWorktree) claimSlot() error {
	if g.slot > 0 {
		return nil
	}
	dir, err := slotsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	for n := 1; n <= maxSlots; n++ {
		f, err := os.OpenFile(filepath.Join(dir, strconv.Itoa(n)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			continue
		}
		_, _ = f.WriteString(g.worktreePath + "\n")
		_ = f.Close()
		g.slot = n
		return nil
	}
	return fmt.Errorf("no free port slot (all %d in use, see %s)", maxSlots, dir)
}

// slotRecord is what a slot file holds: enough to tear the session down without cs storage
// (used by `cs reset`, which wipes storage first).
type slotRecord struct {
	RepoPath         string        `json:"repo_path"`
	WorktreePath     string        `json:"worktree_path"`
	SessionName      string        `json:"session_name"`
	BranchName       string        `json:"branch_name"`
	BaseCommitSHA    string        `json:"base_commit_sha"`
	IsExistingBranch bool          `json:"is_existing_branch"`
	Slot             int           `json:"slot"`
	Linked           []LinkedState `json:"linked"`
}

// writeSlotRecord refreshes the slot file with the session's current state.
func (g *GitWorktree) writeSlotRecord() {
	dir, err := slotsDir()
	if err != nil || g.slot == 0 {
		return
	}
	data, err := json.Marshal(slotRecord{
		RepoPath:         g.repoPath,
		WorktreePath:     g.worktreePath,
		SessionName:      g.sessionName,
		BranchName:       g.branchName,
		BaseCommitSHA:    g.baseCommitSHA,
		IsExistingBranch: g.isExistingBranch,
		Slot:             g.slot,
		Linked:           g.LinkedStates(),
	})
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(g.slot)), data, 0644); err != nil {
		log.ErrorLog.Printf("failed to write slot record: %v", err)
	}
}

// cleanupLinkedSessions fully tears down every linked-repo session that holds a slot:
// teardown hook, linked worktrees and the branches it created, then the slot itself.
func cleanupLinkedSessions() {
	dir, err := slotsDir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		var rec slotRecord
		if err != nil || json.Unmarshal(data, &rec) != nil || rec.RepoPath == "" {
			_ = os.Remove(path)
			continue
		}
		g := NewGitWorktreeFromStorage(rec.RepoPath, rec.WorktreePath, rec.SessionName, rec.BranchName, rec.BaseCommitSHA, rec.IsExistingBranch)
		g.SetLinkedStates(rec.Linked)
		g.SetSlot(rec.Slot)
		if err := g.Cleanup(); err != nil {
			log.ErrorLog.Printf("reset: cleanup of %s: %v", rec.SessionName, err)
		}
		_ = os.Remove(path)
	}
}

// OpenSession finds the multi-repo session whose folder contains dir (e.g. the shell's working
// directory inside a session) from its slot record, so CLI commands can act on it while the
// cs UI runs. dir may be inside a linked repo's folder.
func OpenSession(dir string) (*GitWorktree, error) {
	slots, err := slotsDir()
	if err != nil {
		return nil, err
	}
	entries, _ := os.ReadDir(slots)
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	dir = resolve(dir)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(slots, e.Name()))
		var rec slotRecord
		if err != nil || json.Unmarshal(data, &rec) != nil || rec.WorktreePath == "" {
			continue
		}
		g := NewGitWorktreeFromStorage(rec.RepoPath, rec.WorktreePath, rec.SessionName, rec.BranchName, rec.BaseCommitSHA, rec.IsExistingBranch)
		g.SetLinkedStates(rec.Linked)
		g.SetSlot(rec.Slot)
		roots := []string{resolve(rec.WorktreePath)}
		for _, l := range g.links() {
			roots = append(roots, resolve(l.worktreePath))
		}
		for _, root := range roots {
			if dir == root || strings.HasPrefix(dir, root+string(os.PathSeparator)) {
				return g, nil
			}
		}
	}
	return nil, fmt.Errorf("%s is not inside a claude-squad multi-repo session", dir)
}

func (g *GitWorktree) releaseSlot() {
	if g.slot == 0 {
		return
	}
	if dir, err := slotsDir(); err == nil {
		_ = os.Remove(filepath.Join(dir, strconv.Itoa(g.slot)))
	}
	g.slot = 0
}

func (g *GitWorktree) runHook(name string) {
	script := filepath.Join(g.repoPath, HooksDir, name)
	if _, err := os.Stat(script); err != nil {
		return
	}
	env := append(os.Environ(),
		"CS_SESSION="+g.sessionName,
		"CS_BRANCH="+g.branchName,
		"CS_SLOT="+strconv.Itoa(g.slot),
		"CS_SESSION_DIR="+g.worktreePath,
	)
	for _, l := range g.links() {
		key := strings.ToUpper(strings.ReplaceAll(l.alias, "-", "_"))
		env = append(env, "CS_REPO_"+key+"="+l.worktreePath, "CS_MAIN_"+key+"="+l.repoPath)
	}
	logPath := g.worktreePath + "." + name + ".log"
	logFile, err := os.Create(logPath)
	if err != nil {
		log.ErrorLog.Printf("hook %s: %v", name, err)
		return
	}
	defer logFile.Close()
	cmd := exec.Command(script)
	cmd.Dir = g.repoPath
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Run(); err != nil {
		log.ErrorLog.Printf("hook %s failed (see %s): %v", name, logPath, err)
	}
}

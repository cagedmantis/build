// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/build/gerrit"
)

// gitCheckout sets up a fresh git checkout in which to work,
// in $HOME/go-releasebot-work/<release>/gitwork
// (where <release> is a string like go1.8.5).
// The first time it is run for a particular release,
// gitCheckout also creates a clean checkout in
// $HOME/go-releasebot-work/<release>/gitmirror,
// to use as an object cache to speed future checkouts.
// On return, w.runDir has been set to gitwork/src,
// to allow commands like "./make.bash".
func (w *Work) gitCheckout() {
	shortRel := strings.ToLower(w.Milestone.GetTitle())
	shortRel = shortRel[:strings.LastIndex(shortRel, ".")]
	w.ReleaseBranch = "release-branch." + shortRel
	//TODO: move to go-releasebot-work
	w.Dir = filepath.Join(os.Getenv("HOME"), "go-releasebot-work/"+strings.ToLower(w.Milestone.GetTitle()))
	w.log.Printf("working in %s\n", w.Dir)
	if err := os.MkdirAll(w.Dir, 0777); err != nil {
		w.log.Panic(err)
	}

	// Check out a local mirror to work-mirror, to speed future checkouts for this point release.
	mirror := filepath.Join(w.Dir, "gitmirror")
	if _, err := os.Stat(mirror); err != nil {
		w.run("git", "clone", "https://go.googlesource.com/go", mirror)
		w.runDir = mirror
		w.run("git", "config", "gc.auto", "0") // don't throw away refs we fetch
	} else {
		w.runDir = mirror
		w.run("git", "fetch", "origin", "master")
	}
	w.run("git", "fetch", "origin", w.ReleaseBranch)

	// Clone real Gerrit, but using local mirror for most objects.
	gitDir := filepath.Join(w.Dir, "gitwork")
	if err := os.RemoveAll(gitDir); err != nil {
		w.log.Panic(err)
	}
	w.run("git", "clone", "--reference", mirror, "-b", w.ReleaseBranch, "https://go.googlesource.com/go", gitDir)
	w.runDir = gitDir
	w.run("git", "change", "relwork")
	w.run("git", "config", "gc.auto", "0") // don't throw away refs we fetch
	w.runDir = filepath.Join(gitDir, "src")

	version := strings.ToLower(w.Milestone.GetTitle())
	_, err := w.runErr("git", "rev-parse", version)
	if err == nil && !w.CloseMilestone {
		w.logError(nil, fmt.Sprintf("%s tag already exists in Go repository!", version))
		w.log.Panic("already released")
	}
	if err != nil && w.CloseMilestone {
		w.log.Panic("not yet released")
	}
}

// gitFetchCLs fetches into gitwork the commits of each CL in w.CLs.
// It also initializes cl.Order to a numerically increasing ordering that
// respects git commit sequencing. CLs already merged into the master branch
// are ordered before CLs that are pending or found on other branches.
func (w *Work) gitFetchCLs() {
	args := []string{"git", "fetch", "origin"}
	args = append(args, "master:gerrit/master", w.ReleaseBranch+":gerrit/"+w.ReleaseBranch)
	for _, cl := range w.CLs {
		if cl.Ref != "" {
			args = append(args, cl.Ref+":gerrit/"+cl.Ref)
		}
	}
	w.runOut(args...)

	order := make(map[string]int)
	for _, cl := range w.CLs {
		order[cl.Commit] = -1
	}
	next := 0
	for _, ref := range args[3:] {
		ref := ref[strings.Index(ref, ":")+1:]
		lines := strings.Split(string(w.runOut("git", "log", "--pretty=format:%H", ref)), "\n")
		n := 0
		for _, line := range lines {
			if order[line] > 0 {
				break
			}
			if order[line] == -1 {
				n++
			}
		}
		n += next
		next = n
		for _, line := range lines {
			if order[line] > 0 {
				break
			}
			if order[line] == -1 {
				order[line] = n
				n--
			}
		}
	}
	for _, cl := range w.CLs {
		cl.Order = order[cl.Commit]
	}
}

// orderCLs decides the order in which to apply CLs to the release branch.
// The order chosen is the original commit order recorded by gitFetchCLs,
// with prerequisites specified in the issue directives pulled in eagerly.
//
// For example, suppose we want to pick CLs 1 2 3 4 5 from master
// along with pending CL 6, which is a replacement for a CL from master
// that happened between 2 and 3. The normal order we'd choose would
// be 1 2 3 4 5 6, but if the issue directive OKing CL 3 says:
//
//	CL 3 OK for Go 1.9.2 (after CL 6).
//
// then CL 6 will be inserted ahead of where 3 would normally be chosen,
// leading to 1 2 6 3 4 5.
//
// An alternative would be to delay 3 until 6 had come up normally,
// producing 1 2 4 5 6 3, but in general these new CLs are rewrites to
// replace older CLs, so sliding individual new CLs earlier and therefore
// preserving the original master order (in this case, keeping 3 before 4
// without having to say so explicitly) typically works better.
func (w *Work) orderCLs() {
	cls := w.CLs
	sort.Slice(cls, func(i, j int) bool { return cls[i].Order < cls[j].Order })

	var order []*CL
	walking := make(map[*CL]bool)
	walked := make(map[*CL]bool)
	clByNum := map[int]*CL{}
	for _, cl := range cls {
		clByNum[cl.Num] = cl
	}
	var walk func(cl *CL)
	walk = func(cl *CL) {
		if walked[cl] {
			return
		}
		if walking[cl] {
			w.log.Panic("CL cycle")
		}
		walking[cl] = true
		for _, prereq := range cl.Prereq {
			if clByNum[prereq] == nil {
				w.log.Panicf("CL %d has prereq non-approved CL %d", cl.Num, prereq)
			}
			walk(clByNum[prereq])
		}
		order = append(order, cl)
		walked[cl] = true
	}
	for _, cl := range cls {
		walk(cl)
	}
	if len(order) != len(cls) {
		w.log.Panic("dropped CLs during ordering")
	}
	copy(cls, order)
	w.CLs = cls
}

// cherryPickCLs applies the CLs, in the order chosen by orderCLs, to the release branch.
// If a cherry-pick fails, the error is recorded but it does not stop the overall process,
// so that (especially early in the process), a cherry-pick failure in one directory does not
// keep us from finding out whether cherry-picks in other directories work.
// After each chery-pick, cherryPickCLs checks to see if there is a corresponding Gerrit CL
// already, and if it has exactly the same parent and file content, cherryPickCLs reuses
// that existing CL, to reduce the number of uploads and trybot runs.
// Before deciding to keep a cherry-pick, cherryPickCLs runs make.bash to make sure
// that the resulting tree at least compiles. If not, the error is recorded, the cherry-pick
// is rolled back, and the process continues. If the cherry-pick was an exact match for
// a pending Gerrit CL and that CL has a TryBot +1 vote, the make.bash is skipped.
// After creating each CL not already on Gerrit, cherryPickCLs mails the CL to Gerrit
// and asks for a trybot run.
//
// The result of all this is that provided the CL sequence is already on Gerrit and has
// passed all its trybot runs, cherryPickCLs runs fairly quickly and is idempotent:
// for each CL, it does only the git cherry-pick, finds the CL already on Gerrit,
// with a TryBot +1, and moves on to the next one.
//
// Note that CLs can be cherry-picked from master or pulled in from pending work
// on the release branch. In the latter case, releasebot essentially adopts the pending CL,
// pushing new revisions that set it into the right place in the overall stack.
func (w *Work) cherryPickCLs() {
	lastRef := w.ReleaseBranch
	lastCommit := "origin/" + w.ReleaseBranch

	for _, cl := range w.CLs {
		w.log.Printf("# CL %d\n", cl.Num)
		if cl.Commit == "" {
			w.log.Printf("SKIP - missing commit\n")
			continue
		}

		_, err := w.runErr("git", "cherry-pick", cl.Commit)
		if err != nil {
			w.logError(cl, fmt.Sprintf("git cherry-pick failed:\n\n"+
				"    git fetch origin %s\n"+
				"    git checkout %s\n"+
				"    git fetch origin %s\n"+
				"    git cherry-pick %s",
				lastRef, lastCommit, cl.Ref, cl.Commit))
			w.run("git", "cherry-pick", "--abort")
			continue
		}
		w.run("git", "commit", "--amend") // commit hook puts [release-branch] prefix in

		// Search Gerrit to find any pre-existing CL we'd be updating by doing a git mail.
		// If one exists and it has the same parent, tree, and commit message as our local
		// commit, the only difference is the author/committer lines and (more likely) time stamps.
		// Don't bother pushing a new CL just to change those.
		change := cl.ReleaseBranchGerrit
		if change != nil {
			log.Printf("CHECK %d\n", change.ChangeNumber)
			ref := change.Revisions[change.CurrentRevision].Ref
			w.runOut("git", "fetch", "origin", ref)
			tree1, parent1 := w.treeAndParentOfCommit("FETCH_HEAD")
			tree2, parent2 := w.treeAndParentOfCommit("HEAD")
			if tree1 == tree2 && parent1 == parent2 {
				w.log.Printf("reusing existing %s for CL %d", ref, cl.Num)
				w.run("git", "reset", "--hard", "FETCH_HEAD")
				cl.ReleaseBranchGerrit = change
			} else {
				log.Printf("CHECK %d fail\n", change.ChangeNumber)
				change = nil
			}
		} else {
			log.Printf("NO GERRIT %d\n", cl.Num)
		}

		// Before pushing to Gerrit, check that make.bash works.
		// If we have a pre-existing CL we're adopting and the trybots
		// say it's OK, that's even better, so skip make.bash.
		if change != nil && labelValue(change, "TryBot-Result") >= +1 {
			w.log.Printf("found trybot OK on Gerrit; skipping make.bash")
		} else {
			_, err = w.runErr("./make.bash")
			if err != nil {
				w.logError(cl, fmt.Sprintf("make.bash after git cherry-pick failed:\n\n"+
					"    git fetch origin %s\n"+
					"    git checkout %s\n"+
					"    git fetch origin %s\n"+
					"    git cherry-pick %s\n"+
					"    ./make.bash\n",
					lastRef, lastCommit, cl.Ref, cl.Commit))
				w.run("git", "reset", "--hard", "HEAD^")
				continue
			}
		}

		// Push to Gerrit.
		if change == nil {
			w.run("git", "mail", "-trybot", "HEAD")
			change = w.topGerritCL()
		}
		cl.ReleaseBranchCL = change.ChangeNumber
		cl.ReleaseBranchGerrit = change

		if labelValue(change, "Code-Review") < +2 {
			w.logError(cl, "missing Code-Review +2")
		}

		lastRef = change.Revisions[change.CurrentRevision].Ref
		lastCommit = change.CurrentRevision

		w.updateSummary()
	}
}

// gitTagVersion tags the release candidate or release in Git.
func (w *Work) gitTagVersion() {
	w.runDir = filepath.Join(w.Dir, "gitwork")
	if w.FinalRelease {
		w.run("git", "submit", "-i") // EDITOR=true so submits everything
		w.run("git", "sync")
	}

	out, err := w.runErr("git", "tag", w.Version)
	if err != nil {
		w.logError(nil, fmt.Sprintf("git tag failed: %s\n%s", err, out))
		return
	}
	w.run("git", "push", "origin", w.Version)
}

// topChangeID returns the Change-Id line of the top-most commit in the git client.
func (w *Work) topChangeID() string {
	cmd := exec.Command("git", "cat-file", "commit", "HEAD")
	cmd.Dir = w.runDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.log.Printf("git cat-file commit HEAD failed: %s\n%s", err, out)
		panic("git")
	}
	var id string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			id = ""
		}
		if strings.HasPrefix(line, "Change-Id: ") {
			id = strings.TrimPrefix(line, "Change-Id: ")
		}
	}
	if id == "" {
		w.log.Panic("cannot find Change-Id in HEAD")
	}
	return id
}

// topGerritCL returns the Gerrit information for the top-most commit in the git client.
func (w *Work) topGerritCL() *gerrit.ChangeInfo {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = w.runDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.log.Printf("git rev-parse HEAD failed: %s\n%s", err, out)
		panic("git")
	}
	changes, err := gerritClient.QueryChanges(context.TODO(), "commit:"+strings.TrimSpace(string(out)), gerrit.QueryChangesOpt{Fields: []string{"LABELS", "CURRENT_REVISION"}})
	if err != nil {
		w.log.Panic(err)
	}
	if len(changes) != 1 {
		w.log.Panic("cannot find git HEAD on Gerrit")
	}
	return changes[0]
}

// treeAndParentOfCommit returns the tree and parent hashes
// for the given commit.
func (w *Work) treeAndParentOfCommit(commit string) (tree, parent string) {
	out := w.runOut("git", "cat-file", "commit", commit)
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "tree ") {
			tree = strings.TrimPrefix(line, "tree ")
		}
		if strings.HasPrefix(line, "parent ") {
			parent = strings.TrimPrefix(line, "parent ")
		}
		if line == "" {
			break
		}
	}
	if tree == "" || parent == "" {
		w.log.Panicf("getCommitInfo %s: malformed commit blob:\n%s", commit, out)
	}
	return
}
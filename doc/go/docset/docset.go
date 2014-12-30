// docset keeps track of checkouts of a repository of Markdown documents.
package docset

/*

  DocSets work around a `workDir` which is presumed to be a directory with enough space
  to handle many checkouts of the Markdown repository. It has the following structure:

  {workDir}
    /primary/ - The primary checkout of the Markdown repository.
    /patches/
       {issue_id}-{patchset_id}/ - A patched checkout of the Markdown respository.

  Each repo should have a directory /site that contains all the documentation in Markdown
  and associated files such as PNG images, For example:

  site
  ├── dev
  │   ├── contrib
  │   │   ├── codereviews.md
  │   │   └── index.md
  │   └── index.md
  ├── index.md
  ├── logo.png
  ├── roles.md
  ├── roles.png
  ├── user
  │   ├── api.md
  │   ├── download.md
  │   ├── index.md
  │   ├── issue-tracker.md
  │   └── quick
  │       ├── index.md
  │       └── linux.md
  └── xtra
      └── index.md

*/

import (
	"bufio"
	"fmt"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"

	"skia.googlesource.com/buildbot.git/doc/go/config"
	"skia.googlesource.com/buildbot.git/doc/go/reitveld"
	"skia.googlesource.com/buildbot.git/go/gitinfo"
	"skia.googlesource.com/buildbot.git/go/util"
)

var (
	rc = reitveld.NewClient()
)

// DocSet is a single checked out repository of Markdown documents.
type DocSet struct {
	// repoDir is the directory the repo is checked out into.
	repoDir string
	// navigation is the HTML formatted navigation structure for the given repo.
	navigation string

	mutex sync.Mutex
}

// newDocSet does the core of the work for both NewDocSet and NewDocSetForIssue.
//
// The repo is checked out into repoDir.
// If a valid issue and patchset are supplied then the repo will be patched with that CL.
// If refresh is true then the git repo will be periodically refreshed (git pull).
func newDocSet(repoDir, repo string, issue, patchset int64, refresh bool) (*DocSet, error) {
	git, err := gitinfo.CloneOrUpdate(repo, repoDir, false)
	if err != nil {
		glog.Fatalf("Failed to CloneOrUpdate repo %q: %s", repo, err)
	}
	if issue > 0 {
		cmd := exec.Command("patch", "-p1")
		cmd.StdinPipe()
		cmd.Dir = repoDir
		diff, err := rc.Patchset(issue, patchset)
		if err != nil {
			return nil, fmt.Errorf("Failed to retrieve patchset: %s", err)
		}
		cmd.Stdin = diff
		defer diff.Close()
		b, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("Error while patching %#v - %s: %s", *cmd, string(b), err)
		}
	}
	d := &DocSet{
		repoDir: repoDir,
	}
	d.BuildNavigation()
	if refresh {
		go func() {
			for _ = range time.Tick(config.REFRESH) {
				git.Update(true, false)
				d.BuildNavigation()
			}
		}()
	}
	return d, nil
}

// NewDocSet creates a new DocSet, one that is periodically refreshed.
func NewDocSet(workDir, repo string) (*DocSet, error) {
	d, err := newDocSet(filepath.Join(workDir, "primary"), repo, -1, -1, true)
	if err != nil {
		return nil, fmt.Errorf("Failed to CloneOrUpdate repo %q: %s", repo, err)
	}
	return d, nil
}

// NewDocSetForIssue creates a new DocSet patched to the latest patch level of
// the given issue.
//
// The returned DocSet is not periodically refreshed.
func NewDocSetForIssue(workDir, repo string, issue int64) (*DocSet, error) {
	// Only do pull and patch if directory doesn't exist.
	issueInfo, err := rc.Issue(issue)
	if err != nil {
		return nil, fmt.Errorf("Failed to load issue info: %s", err)
	}
	patchset := issueInfo.Patchsets[len(issueInfo.Patchsets)-1]
	addr, err := mail.ParseAddress(issueInfo.OwnerEmail)
	if err != nil {
		return nil, fmt.Errorf("CL contains invalid author email: %s", err)
	}
	domain := strings.Split(addr.Address, "@")[1]
	if !util.In(domain, config.WHITELIST) {
		return nil, fmt.Errorf("User is not authorized to test docset CLs.")
	}
	var d *DocSet
	repoDir := filepath.Join(workDir, "patches", fmt.Sprintf("%d-%d", issue, patchset))
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		d, err = newDocSet(repoDir, repo, issue, patchset, false)
		if err != nil {
			if err := os.RemoveAll(repoDir); err != nil {
				glog.Errorf("Failed to remove %q: %s", repoDir, err)
			}
			return nil, fmt.Errorf("Failed to create new doc set: %s", err)
		}
	} else {
		d = &DocSet{
			repoDir: repoDir,
		}
		d.BuildNavigation()
	}
	return d, nil
}

// RawFilename returns the absolute filename for the file associated with the
// given url.
//
// The bool returned will be true if the url identifies a raw resource, such as
// a PNG, as opposed to a Markdown file that should be processed.
func (d *DocSet) RawFilename(url string) (string, bool, error) {
	startFilename := filepath.Join(d.repoDir, config.REPO_SUBDIR, url)
	endFilename, err := findFile(startFilename)
	return endFilename, (startFilename == endFilename), err
}

// buildNavString converts a slice of navEntry's into an HTML formatted
// navigation structure.
func buildNavString(nav []*navEntry) string {
	res := ""
	parent := ""    // The parent directory.
	hasSub := false // Is true if a directory contains a non-index.md Markdown file.
	for _, n := range nav {
		// If we are still descending this tree.
		if strings.HasPrefix(n.URL, parent) {
			// If this is a new sub-directory.
			if strings.HasSuffix(n.URL, "/") {
				// If we have previously emitted a non-index.md Markdown file in the current directory.
				if hasSub {
					res += "</ul>\n"
				}
				res += "<ul>\n"
				parent = n.URL
				hasSub = false
			} else {
				// If this is the first non-index.md file in the current directory.
				if !hasSub {
					hasSub = true
					res += "<ul>\n"
				}
			}
		} else {
			num := strings.Count(parent, "/")
			for i := 0; i < num-1; i++ {
				res += "</ul>\n"
			}
			// If we have previously emitted a non-index.md Markdown file in the current directory.
			if hasSub {
				res += "</ul>\n"
			}
			hasSub = false
			res += "<ul>\n"
			parent = n.URL
		}
		res += fmt.Sprintf("<li><a data-path=\"%s\" href=\"%s\">%s</a></li>\n", n.URL, n.URL, n.Name)
	}
	// Close out all remaining open ul's.
	num := strings.Count(parent, "/")
	for i := 0; i < num; i++ {
		res += "</ul>\n"
	}
	if hasSub {
		res += "</ul>\n"
	}
	return res
}

// BuildNavigation builds the Navigation for the DocSet.
func (d *DocSet) BuildNavigation() {
	// Walk the directory tree to build the navigation menu.
	nav := []*navEntry{}
	root := filepath.Join(d.repoDir, config.REPO_SUBDIR)
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		rel, _ := filepath.Rel(root, path)
		rel = "/" + rel
		if strings.HasSuffix(rel, "/index.md") {
			rel = rel[:len(rel)-8]
		} else if strings.HasSuffix(rel, ".md") {
			rel = rel[:len(rel)-3]
		} else {
			return nil
		}
		nav = append(nav, &navEntry{
			URL:  rel,
			Name: readTitle(path, rel),
		})
		return nil
	})
	sort.Sort(navEntrySlice(nav))
	s := buildNavString(nav)
	d.mutex.Lock()
	defer d.mutex.Unlock()
	d.navigation = s
}

// Navigation returns the HTML formatted navigation.
func (d *DocSet) Navigation() string {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	return d.navigation
}

// issueAndPatch is a regex for extracting the issue number from a directory name
// that is formatted like {issue_id}-{pathset_id}.
var issueAndPatch = regexp.MustCompile("([0-9]+)-[0-9]+")

// StartCleaner is a process that periodically checks the status of every issue
// that has been previewed and removes all the local files for closed issues.
func StartCleaner(workDir string) {
	glog.Info("Starting Cleaner")
	c := reitveld.NewClient()
	for _ = range time.Tick(config.REFRESH) {
		matches, err := filepath.Glob(workDir + "/patches/*")
		glog.Infof("Matches: %v", matches)
		if err != nil {
			glog.Errorf("Failed to retrieve list of patched checkouts: %s", err)
			continue
		}
		for _, filename := range matches {
			_, file := filepath.Split(filename)
			glog.Info(file)
			m := issueAndPatch.FindStringSubmatch(file)
			if len(m) < 2 {
				continue
			}
			issue, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil {
				glog.Errorf("Failed to parse %q as int: %s", m[1], err)
				continue
			}
			issueInfo, err := c.Issue(issue)
			if err != nil {
				glog.Errorf("Failed to retrieve issue status %d: %s", issue, err)
			}
			if issueInfo.Closed {
				if err := os.RemoveAll(filename); err != nil {
					glog.Errorf("Failed to remove %q: %s", filename, err)
				}
			}
		}
	}
}

// findFile takes a filename guess and turns into real name of a file.
//
// Look for the given file, if it exists then serve it raw with a guess at the
// content type. Otherwise append ".md" and return it as processed markdown.
// If it is a directory append "index.md" and return that.
func findFile(filename string) (string, error) {
	if stat, err := os.Stat(filename); err == nil {
		if stat.IsDir() {
			return findFile(filepath.Join(filename, "./index.md"))
		} else {
			return filename, nil
		}
	} else {
		if filepath.Ext(filename) == ".md" {
			return filename, err
		} else {
			return findFile(filename + ".md")
		}
	}
}

// pathOf returns a '/' terminated path for the given filename.
func pathOf(s string) string {
	if s[len(s)-1] == '/' {
		return s
	}
	parts := strings.Split(s, "/")
	if len(parts) > 0 {
		parts = parts[:len(parts)-1]
	}

	ret := strings.Join(parts, "/")
	if len(ret) == 0 || ret[len(ret)-1] != '/' {
		ret += "/"
	}
	return ret
}

// navEntry is a single directory entry in the Markdown repo.
type navEntry struct {
	URL  string
	Name string
}

// navEntrySlice is a utility type for sorting navEntry's.
type navEntrySlice []*navEntry

func (p navEntrySlice) Len() int { return len(p) }
func (p navEntrySlice) Less(i, j int) bool {
	if pathOf(p[i].URL) < pathOf(p[j].URL) {
		return true
	}
	if pathOf(p[i].URL) == pathOf(p[j].URL) {
		return p[i].URL < p[j].URL
	}
	return false
}
func (p navEntrySlice) Swap(i, j int) { p[i], p[j] = p[j], p[i] }

// readTitle reads the first line from a Markdown file.
func readTitle(filename, def string) string {
	f, err := os.Open(filename)
	if err != nil {
		glog.Warningf("Failed to open file %s: %s", filename, err)
		return def
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	title, err := reader.ReadString('\n')
	if err != nil {
		glog.Warningf("Failed to read title %s: %s", filename, err)
	}
	return strings.TrimSpace(title)
}
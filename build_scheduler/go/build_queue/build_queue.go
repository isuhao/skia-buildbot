package build_queue

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/skia-dev/glog"
	"go.skia.org/infra/build_scheduler/go/blacklist"
	"go.skia.org/infra/go/buildbot"
	"go.skia.org/infra/go/common"
	"go.skia.org/infra/go/git/repograph"
	"go.skia.org/infra/go/timer"
	"go.skia.org/infra/go/util"
)

const (
	// Default score threshold for scheduling builds. This is "essentially zero",
	// allowing for significant floating point error, which indicates that we will
	// backfill builds for all commits except for those at which we've already built.
	DEFAULT_SCORE_THRESHOLD = 0.0001

	// Don't bisect builds with greater than this many commits. This
	// prevents spending lots of time computing giant blamelists.
	NO_BISECT_COMMIT_LIMIT = 100

	// If this time period used, include commits from the beginning of time.
	PERIOD_FOREVER = 0
)

var (
	// "Constants".

	// Blacklist these branches.
	BLACKLIST_BRANCHES = []string{
		"infra/config",
	}

	// ERR_EMPTY_QUEUE is returned by BuildQueue.Pop() when the queue for
	// that builder is empty.
	ERR_EMPTY_QUEUE = fmt.Errorf("Queue is empty.")
)

// BuildCandidate is a struct which describes a candidate for a build.
type BuildCandidate struct {
	Commit  *repograph.Commit
	Builder string
	Score   float64
	Repo    string
}

// BuildCandidateSlice is an alias to help sort BuildCandidates.
type BuildCandidateSlice []*BuildCandidate

func (s BuildCandidateSlice) Len() int { return len(s) }
func (s BuildCandidateSlice) Less(i, j int) bool {
	if s[i].Score == s[j].Score {
		// Fall back to sorting by commit hash to keep the sort
		// order consistent for testing.
		return s[i].Commit.Hash < s[j].Commit.Hash
	}
	return s[i].Score < s[j].Score
}
func (s BuildCandidateSlice) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

// BuildQueue is a struct which contains a priority queue for builders and
// commits.
type BuildQueue struct {
	bl               *blacklist.Blacklist
	db               buildbot.DB
	lock             sync.RWMutex
	period           time.Duration
	scoreThreshold   float64
	queue            map[string][]*BuildCandidate
	recentCommits    []string
	recentCommitsMtx sync.RWMutex
	repos            repograph.Map
	timeLambda       float64
}

// NewBuildQueue creates and returns a BuildQueue instance which considers
// commits in the specified time period.
//
// Build candidates with a score below the given scoreThreshold are not added
// to the queue. The score for a build candidate is defined as the value added
// by running that build, which is the difference between the total scores for
// all commits on a given builder before and after the build candidate would
// run. Scoring for an individual commit/builder pair is as follows:
//
// -1.0    if no build has ever included this commit on this builder.
// 1.0     if this builder has built AT this commit.
// 1.0 / N if a build on this builer has included this commit, where N is the
//         number of commits included in the build.
//
// The scoring works out such that build candidates which include commits which
// have never been included in a build have a value-add of >= 2, and other build
// candidates (eg. backfilling) have a value-add of < 2.
//
// Additionally, the scores include a time factor which serves to prioritize
// backfilling of more recent commits. The time factor is an exponential decay
// which is controlled by the timeDecay24Hr parameter. This parameter indicates
// what the time factor should be after 24 hours. For example, setting
// timeDecay24Hr equal to 0.5 causes the score for a build candidate with value
// 1.0 to be 0.5 if the commit is 24 hours old. At 48 hours with a value of 1.0
// the build candidate would receive a score of 0.25.
func NewBuildQueue(period time.Duration, repos repograph.Map, scoreThreshold, timeDecay24Hr float64, bl *blacklist.Blacklist, db buildbot.DB) (*BuildQueue, error) {
	if timeDecay24Hr <= 0.0 || timeDecay24Hr > 1.0 {
		return nil, fmt.Errorf("Time penalty must be 0 < p <= 1")
	}
	q := &BuildQueue{
		bl:               bl,
		db:               db,
		lock:             sync.RWMutex{},
		period:           period,
		scoreThreshold:   scoreThreshold,
		queue:            map[string][]*BuildCandidate{},
		recentCommits:    []string{},
		recentCommitsMtx: sync.RWMutex{},
		repos:            repos,
		timeLambda:       lambda(timeDecay24Hr),
	}
	return q, nil
}

// lambda returns the lambda-value given a decay amount at 24 hours.
func lambda(decay float64) float64 {
	return (-math.Log(decay) / float64(24))
}

// timeFactor returns the time penalty factor, which is an exponential decay.
func timeFactor(now, t time.Time, lambda float64) float64 {
	hours := float64(now.Sub(t)) / float64(time.Hour)
	return math.Exp(-lambda * hours)
}

// scoreBuild returns the current score for the given commit/builder pair. The
// details on how scoring works are described in the doc for NewBuildQueue.
func scoreBuild(commit *repograph.Commit, build *buildbot.Build, now time.Time, timeLambda float64) float64 {
	s := -1.0
	if build != nil {
		if build.GotRevision == commit.Hash {
			s = 1.0
		} else if util.In(commit.Hash, build.Commits) {
			s = 1.0 / float64(len(build.Commits))
		}
	}
	return s * timeFactor(now, commit.Timestamp, timeLambda)
}

// RecentCommits returns a list of recent commit hashes.
func (q *BuildQueue) RecentCommits() []string {
	q.recentCommitsMtx.RLock()
	defer q.recentCommitsMtx.RUnlock()
	return q.recentCommits
}

// setRecentCommits sets the list of recent commit hashes.
func (q *BuildQueue) setRecentCommits(c []string) {
	q.recentCommitsMtx.Lock()
	defer q.recentCommitsMtx.Unlock()
	q.recentCommits = c
}

// Update retrieves the set of commits over a time period and the builds
// associated with those commits and builds a priority queue for commit/builder
// pairs.
func (q *BuildQueue) Update() error {
	return q.update(time.Now())
}

// update is the inner function which does all the work for Update. It accepts
// a time.Time so that time.Now() can be faked for testing.
func (q *BuildQueue) update(now time.Time) error {
	glog.Info("Updating build queue.")
	defer timer.New("BuildQueue.update()").Stop()
	queue := map[string][]*BuildCandidate{}
	errs := map[string]error{}
	mutex := sync.Mutex{}
	var wg sync.WaitGroup
	for repoUrl, repo := range q.repos {
		wg.Add(1)
		go func(repo *repograph.Graph, repoUrl string) {
			defer wg.Done()
			candidates, err := q.updateRepo(repo, now)
			mutex.Lock()
			defer mutex.Unlock()
			if err != nil {
				errs[repoUrl] = err
				return
			}
			for k, v := range candidates {
				queue[k] = v
			}
		}(repo, repoUrl)
	}
	wg.Wait()
	if len(errs) > 0 {
		msg := "Failed to update repos:"
		for repoUrl, err := range errs {
			msg += fmt.Sprintf("\n%s: %v", repoUrl, err)
		}
		return fmt.Errorf(msg)
	}

	// Update the queues.
	q.lock.Lock()
	defer q.lock.Unlock()
	q.queue = queue

	return nil
}

// updateRepo syncs the given repo and returns a set of BuildCandidates for
// each builder which uses it.
func (q *BuildQueue) updateRepo(repo *repograph.Graph, now time.Time) (map[string][]*BuildCandidate, error) {
	defer timer.New("BuildQueue.updateRepo()").Stop()
	errMsg := "Failed to update the repo: %v"

	// Sync/update the code.
	if err := repo.Update(); err != nil {
		return nil, fmt.Errorf(errMsg, err)
	}

	// Get the details for all recent commits.
	from := now.Add(-q.period)
	if q.period == PERIOD_FOREVER {
		from = time.Unix(0, 0)
	}
	// Pre-load builds from a larger window than we actually care about.
	fromPreload := now.Add(time.Duration(int64(-1.5 * float64(q.period))))
	if q.period == PERIOD_FOREVER {
		fromPreload = time.Unix(0, 0)
	}
	// Figure out which branch heads to blacklist.
	commitBlacklist := map[*repograph.Commit]bool{}
	for _, branch := range BLACKLIST_BRANCHES {
		head := repo.Get(branch)
		if head != nil {
			commitBlacklist[head] = true
		}
	}

	// Find recent commits.
	recentCommits := make([]*repograph.Commit, 0, 100)
	recentCommitsPreload := make([]*repograph.Commit, 0, 100)
	if err := repo.RecurseAllBranches(func(c *repograph.Commit) (bool, error) {
		if c.Timestamp.Before(fromPreload) {
			return false, nil
		}
		if commitBlacklist[c] {
			return false, nil
		}
		recentCommitsPreload = append(recentCommits, c)
		if c.Timestamp.After(from) {
			recentCommits = append(recentCommits, c)
		}
		return true, nil
	}); err != nil {
		return nil, err
	}

	// Sort commits lists by timestamp.
	sort.Sort(repograph.CommitSlice(recentCommits))
	sort.Sort(repograph.CommitSlice(recentCommitsPreload))

	// Set the list of recent commit hashes.
	q.setRecentCommits(repograph.CommitSlice(recentCommits).Hashes())

	// Get all builds associated with the recent commits in the preload.
	buildsByCommit, err := q.db.GetBuildsForCommits(repograph.CommitSlice(recentCommitsPreload).Hashes(), nil)
	if err != nil {
		return nil, fmt.Errorf(errMsg, err)
	}

	// Create buildCaches for each builder.
	buildCaches := map[string]*buildCache{}
	for _, buildsForCommit := range buildsByCommit {
		for _, build := range buildsForCommit {
			if rule := q.bl.MatchRule(build.Builder, ""); rule != "" {
				//glog.Warningf("Skipping blacklisted builder: %s due to rule %q", build.Builder, rule)
				continue
			}
			if _, ok := buildCaches[build.Builder]; !ok {
				repo, ok := q.repos[build.Repository]
				if !ok {
					return nil, fmt.Errorf(errMsg, fmt.Sprintf("No such repo: %s", build.Repository))
				}
				bc, err := newBuildCache(build.Master, build.Builder, build.Repository, repo, q.db)
				if err != nil {
					return nil, fmt.Errorf(errMsg, err)
				}
				buildCaches[build.Builder] = bc
			}
			if err := buildCaches[build.Builder].PutBuild(build); err != nil {
				return nil, err
			}
		}
	}

	// Find candidates for each builder.
	candidates := map[string][]*BuildCandidate{}
	errs := map[string]error{}
	mutex := sync.Mutex{}
	var wg sync.WaitGroup
	for builder, finder := range buildCaches {
		wg.Add(1)
		go func(b string, bc *buildCache) {
			defer wg.Done()
			c, err := q.getCandidatesForBuilder(bc, recentCommits, now)
			mutex.Lock()
			defer mutex.Unlock()
			if err != nil {
				errs[b] = err
				return
			}
			candidates[b] = c
		}(builder, finder)
	}
	wg.Wait()
	if len(errs) > 0 {
		msg := "Failed to update the repo:"
		for _, err := range errs {
			msg += fmt.Sprintf("\n%v", err)
		}
		return nil, fmt.Errorf(msg)
	}
	return candidates, nil
}

// getCandidatesForBuilder finds all BuildCandidates for the given builder, in order.
func (q *BuildQueue) getCandidatesForBuilder(bc *buildCache, recentCommits []*repograph.Commit, now time.Time) ([]*BuildCandidate, error) {
	defer timer.New(fmt.Sprintf("getCandidatesForBuilder(%s)", bc.Builder)).Stop()
	repo := bc.Repo
	candidates := []*BuildCandidate{}
	for {
		score, newBuild, stoleFrom, err := q.getBestCandidate(bc, recentCommits, now)
		if err != nil {
			return nil, fmt.Errorf("Failed to get build candidates for %s: %v", bc.Builder, err)
		}
		if score < q.scoreThreshold {
			break
		}
		d := repo.Get(newBuild.GotRevision)
		if d == nil {
			return nil, fmt.Errorf("Got unknown commit: %s", newBuild.GotRevision)
		}
		// "insert" the new build.
		if err := bc.PutBuild(newBuild); err != nil {
			return nil, err
		}
		if stoleFrom != nil {
			if err := bc.PutBuild(stoleFrom); err != nil {
				return nil, err
			}
		}
		candidates = append(candidates, &BuildCandidate{
			Builder: newBuild.Builder,
			Commit:  d,
			Score:   score,
			Repo:    newBuild.Repository,
		})
	}
	return candidates, nil
}

// getBestCandidate finds the best BuildCandidate for the given builder.
func (q *BuildQueue) getBestCandidate(bc *buildCache, recentCommits []*repograph.Commit, now time.Time) (float64, *buildbot.Build, *buildbot.Build, error) {
	errMsg := fmt.Sprintf("Failed to get best candidate for %s: %%v", bc.Builder)
	// Find the current scores for each commit.
	currentScores := map[*repograph.Commit]float64{}
	for _, commit := range recentCommits {
		currentBuild, err := bc.getBuildForCommit(commit.Hash)
		if err != nil {
			return 0.0, nil, nil, fmt.Errorf(errMsg, err)
		}
		currentScores[commit] = scoreBuild(commit, currentBuild, now, q.timeLambda)
	}

	// For each commit/builder pair, determine the score increase obtained
	// by running a build at that commit.
	scoreIncrease := map[*repograph.Commit]float64{}
	newBuildsByCommit := map[*repograph.Commit]*buildbot.Build{}
	stoleFromByCommit := map[*repograph.Commit]*buildbot.Build{}
	foundBranches := map[string]bool{}
	for _, commit := range recentCommits {
		if rule := q.bl.MatchRule(bc.Builder, commit.Hash); rule != "" {
			//glog.Warningf("Skipping blacklisted builder/commit: %s @ %s due to rule %q", bc.Builder, commit.Hash, rule)
			continue
		}
		// Shortcut: Don't bisect builds with a huge number
		// of commits.  This saves lots of time and only affects
		// the first successful build for a bot. Additionally, don't
		// go past the first commit which ran on this bot.
		b, err := bc.getBuildForCommit(commit.Hash)
		if err != nil {
			return 0.0, nil, nil, fmt.Errorf(errMsg, err)
		}
		if b == nil {
			// Don't go past the first commit on this branch which ran on this bot.
			foundNewerBuild := false
			for branch, _ := range commit.Branches {
				if foundBranches[branch] {
					foundNewerBuild = true
					break
				}
			}
			if foundNewerBuild {
				glog.Warningf("Skipping %s on %s; reached the beginning of time for this bot.", commit.Hash, bc.Builder)
				break
			}
		} else {
			for branch, _ := range commit.Branches {
				foundBranches[branch] = true
			}

			// Don't bisect giant blamelists...
			if len(b.Commits) > NO_BISECT_COMMIT_LIMIT {
				glog.Warningf("Skipping %s on %s; previous build has too many commits (#%d)", commit.Hash[0:7], b.Builder, b.Number)
				scoreIncrease[commit] = 0.0
				break // Don't bother looking at previous commits either, since these will be out of range.
			}
		}

		newScores := map[*repograph.Commit]float64{}
		// Pretend to create a new Build at the given commit.
		newBuild := buildbot.Build{
			Builder:     bc.Builder,
			Master:      bc.Master,
			Number:      bc.MaxBuildNum + 1,
			GotRevision: commit.Hash,
			Repository:  bc.RepoName,
		}
		commits, stealFrom, stolen, err := buildbot.FindCommitsForBuild(bc, &newBuild, q.repos)
		if err != nil {
			return 0.0, nil, nil, fmt.Errorf(errMsg, err)
		}
		// Re-score all commits in the new build.
		newBuild.Commits = commits
		for _, hash := range commits {
			d := bc.Repo.Get(hash)
			if d == nil {
				return 0.0, nil, nil, fmt.Errorf("Got unknown commit: %s", hash)
			}
			if _, ok := currentScores[d]; !ok {
				// If this build has commits which are outside of our window,
				// insert them into currentScores to account for them.
				b, err := bc.getBuildForCommit(d.Hash)
				if err != nil {
					return 0.0, nil, nil, fmt.Errorf(errMsg, err)
				}
				score := scoreBuild(d, b, now, q.timeLambda)
				currentScores[d] = score
			}
			newScores[d] = scoreBuild(d, &newBuild, now, q.timeLambda)
		}
		newBuildsByCommit[commit] = &newBuild
		// If the new build includes commits previously included in
		// another build, update scores for commits in the build we stole
		// them from.
		if stealFrom != -1 {
			stoleFromOrig, err := bc.getByNumber(stealFrom)
			if err != nil {
				return 0.0, nil, nil, fmt.Errorf(errMsg, err)
			}
			if stoleFromOrig == nil {
				// The build may not be cached. Fall back on getting it from the DB.
				stoleFromOrig, err = q.db.GetBuildFromDB(bc.Master, bc.Builder, stealFrom)
				if err != nil {
					return 0.0, nil, nil, fmt.Errorf(errMsg, err)
				}
				if err := bc.PutBuild(stoleFromOrig); err != nil {
					return 0.0, nil, nil, err
				}
			}
			// "copy" the build so that we can assign new commits to it
			// without modifying the cached build.
			stoleFromBuild := *stoleFromOrig
			newCommits := []string{}
			for _, c := range stoleFromBuild.Commits {
				if !util.In(c, stolen) {
					newCommits = append(newCommits, c)
				}
			}
			stoleFromBuild.Commits = newCommits
			for _, c := range stoleFromBuild.Commits {
				d := bc.Repo.Get(c)
				if d == nil {
					return 0.0, nil, nil, fmt.Errorf("Got unknown commit: %s", c)
				}
				newScores[d] = scoreBuild(d, &stoleFromBuild, now, q.timeLambda)
			}
			stoleFromByCommit[commit] = &stoleFromBuild
		}
		// Sum the old and new scores.
		oldScoresList := make([]float64, 0, len(newScores))
		newScoresList := make([]float64, 0, len(newScores))
		for c, score := range newScores {
			oldScoresList = append(oldScoresList, currentScores[c])
			newScoresList = append(newScoresList, score)
		}
		oldTotal := util.Float64StableSum(oldScoresList)
		newTotal := util.Float64StableSum(newScoresList)
		scoreIncrease[commit] = newTotal - oldTotal
	}

	// Arrange the score increases by builder.
	candidates := []*BuildCandidate{}
	for commit, increase := range scoreIncrease {
		candidates = append(candidates, &BuildCandidate{
			Commit: commit,
			Score:  increase,
		})
	}
	sort.Sort(BuildCandidateSlice(candidates))
	if len(candidates) == 0 {
		return 0.0, nil, nil, nil
	}
	best := candidates[len(candidates)-1]

	return best.Score, newBuildsByCommit[best.Commit], stoleFromByCommit[best.Commit], nil
}

// Pop retrieves the highest-priority item in the given set of builders and
// removes it from the queue. Returns nil if there are no items in the queue.
func (q *BuildQueue) Pop(builders []string) (*BuildCandidate, error) {
	q.lock.Lock()
	defer q.lock.Unlock()
	var best *BuildCandidate
	for _, builder := range builders {
		s, ok := q.queue[builder]
		if !ok {
			// We don't yet know about this builder. In other words, it hasn't
			// built any commits. Therefore, the highest-priority commit to
			// build is tip-of-tree. Unfortunately, we don't know which repo
			// the bot uses, so we can only say "origin/master" and use the Skia
			// repo as a default.
			r, ok := q.repos[common.REPO_SKIA]
			if !ok {
				return nil, fmt.Errorf("Unknown repo: %s", common.REPO_SKIA)
			}
			commit := r.Get("master")
			if commit == nil {
				return nil, fmt.Errorf("Unable to retrieve commit at HEAD of master.")
			}
			if rule := q.bl.MatchRule(builder, commit.Hash); rule != "" {
				//glog.Warningf("Skipping blacklisted builder/commit: %s @ %s due to rule %q", builder, commit.Hash, rule)
				continue
			}
			best = &BuildCandidate{
				Builder: builder,
				Commit:  commit,
				Repo:    common.REPO_SKIA,
				Score:   math.MaxFloat64,
			}
			q.queue[builder] = []*BuildCandidate{best}
		} else {
			if len(s) > 0 {
				bc := s[0]
				if best == nil || bc.Score > best.Score {
					best = bc
				}
			}
		}
	}
	// Return the highest-priority commit for this builder.
	if best == nil {
		return nil, ERR_EMPTY_QUEUE
	}
	q.queue[best.Builder] = q.queue[best.Builder][1:len(q.queue[best.Builder])]
	return best, nil
}

// TopN returns the top N scored build candidates in the queue.
func (q *BuildQueue) TopN(n int) []*BuildCandidate {
	q.lock.RLock()
	defer q.lock.RUnlock()
	topN := make([]*BuildCandidate, 0, n)
	for _, candidates := range q.queue {
		for _, candidate := range candidates {
			if len(topN) < n {
				topN = append(topN, candidate)
				sort.Sort(sort.Reverse(BuildCandidateSlice(topN)))
			} else if topN[n-1].Score < candidate.Score {
				topN[n-1] = candidate
				sort.Sort(sort.Reverse(BuildCandidateSlice(topN)))
			}
		}
	}
	return topN
}

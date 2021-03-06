package build_queue

import (
	"io/ioutil"
	"math"
	"os"
	"path"
	"testing"
	"time"

	assert "github.com/stretchr/testify/require"
	"go.skia.org/infra/build_scheduler/go/blacklist"
	"go.skia.org/infra/go/buildbot"
	"go.skia.org/infra/go/common"
	"go.skia.org/infra/go/git/repograph"
	"go.skia.org/infra/go/testutils"
	"go.skia.org/infra/go/util"
)

const (
	TEST_AUTHOR  = "Eric Boren (borenet@google.com)"
	TEST_BUILDER = "Test-Ubuntu-GCC-GCE-CPU-AVX2-x86_64-Release-BuildBucket"
	TEST_REPO    = "https://skia.googlesource.com/skia.git"
)

var (
	// The test repo is laid out like this:
	//
	// *   06eb2a58139d3ff764f10232d5c8f9362d55e20f I (HEAD, origin/master)
	// *   ecb424466a4f3b040586a062c15ed58356f6590e F
	// |\
	// | * d30286d2254716d396073c177a754f9e152bbb52 H
	// | * 8d2d1247ef5d2b8a8d3394543df6c12a85881296 G
	// * | 67635e7015d74b06c00154f7061987f426349d9f E
	// * | 6d4811eddfa637fac0852c3a0801b773be1f260d D
	// * | d74dfd42a48325ab2f3d4a97278fc283036e0ea4 C
	// |/
	// *   4b822ebb7cedd90acbac6a45b897438746973a87 B
	// *   051955c355eb742550ddde4eccc3e90b6dc5b887 A
	//
	hashes = map[rune]string{
		'A': "051955c355eb742550ddde4eccc3e90b6dc5b887",
		'B': "4b822ebb7cedd90acbac6a45b897438746973a87",
		'C': "d74dfd42a48325ab2f3d4a97278fc283036e0ea4",
		'D': "6d4811eddfa637fac0852c3a0801b773be1f260d",
		'E': "67635e7015d74b06c00154f7061987f426349d9f",
		'F': "ecb424466a4f3b040586a062c15ed58356f6590e",
		'G': "8d2d1247ef5d2b8a8d3394543df6c12a85881296",
		'H': "d30286d2254716d396073c177a754f9e152bbb52",
		'I': "06eb2a58139d3ff764f10232d5c8f9362d55e20f",
	}
)

type testDB struct {
	db  buildbot.DB
	dir string
}

func (d *testDB) Close(t *testing.T) {
	assert.NoError(t, d.db.Close())
	assert.NoError(t, os.RemoveAll(d.dir))
}

// clearDB initializes the database, upgrading it if needed, and removes all
// data to ensure that the test begins with a clean slate. Returns a testDB
// which must be closed after the test finishes.
func clearDB(t *testing.T) *testDB {
	tempDir, err := ioutil.TempDir("", "build_scheduler_test_")
	assert.NoError(t, err)
	db, err := buildbot.NewLocalDB(path.Join(tempDir, "buildbot.db"))
	assert.NoError(t, err)

	return &testDB{
		db:  db,
		dir: tempDir,
	}
}

func TestLambda(t *testing.T) {
	testutils.SmallTest(t)
	cases := []struct {
		in  float64
		out float64
	}{
		{
			in:  0.0,
			out: math.Inf(1),
		},
		{
			in:  1.0,
			out: 0.0,
		},
		{
			in:  0.5,
			out: 0.028881132523331052,
		},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.out, lambda(tc.in))
	}
}

func TestBuildScoring(t *testing.T) {
	testutils.MediumTest(t)
	testutils.SkipIfShort(t)

	// Load the test repo.
	tr := util.NewTempRepo()
	defer tr.Cleanup()

	remote := path.Join(tr.Dir, "skia.git")
	repo, err := repograph.NewGraph(remote, tr.Dir)
	assert.NoError(t, err)

	details := map[string]*repograph.Commit{}
	for _, h := range hashes {
		d := repo.Get(h)
		assert.NotNil(t, d)
		details[h] = d
	}

	now := details[hashes['I']].Timestamp.Add(1 * time.Hour)
	build1 := &buildbot.Build{
		GotRevision: hashes['A'],
		Commits:     []string{hashes['A'], hashes['B'], hashes['C']},
	}
	cases := []struct {
		commit        *repograph.Commit
		build         *buildbot.Build
		expectedScore float64
		lambda        float64
	}{
		// Built at the given commit.
		{
			commit:        details[hashes['A']],
			build:         build1,
			expectedScore: 1.0,
			lambda:        lambda(1.0),
		},
		// Build included the commit.
		{
			commit:        details[hashes['B']],
			build:         build1,
			expectedScore: 1.0 / 3.0,
			lambda:        lambda(1.0),
		},
		// Build included the commit.
		{
			commit:        details[hashes['C']],
			build:         build1,
			expectedScore: 1.0 / 3.0,
			lambda:        lambda(1.0),
		},
		// Build did not include the commit.
		{
			commit:        details[hashes['D']],
			build:         build1,
			expectedScore: -1.0,
			lambda:        lambda(1.0),
		},
		// Build is nil.
		{
			commit:        details[hashes['A']],
			build:         nil,
			expectedScore: -1.0,
			lambda:        lambda(1.0),
		},
		// Same cases, but with lambda set to something interesting.
		// Built at the given commit.
		{
			commit:        details[hashes['A']],
			build:         build1,
			expectedScore: 0.958902488117383,
			lambda:        lambda(0.5),
		},
		// Build included the commit.
		{
			commit:        details[hashes['B']],
			build:         build1,
			expectedScore: 0.3228038362210165,
			lambda:        lambda(0.5),
		},
		// Build included the commit.
		{
			commit:        details[hashes['C']],
			build:         build1,
			expectedScore: 0.32299553133576475,
			lambda:        lambda(0.5),
		},
		// Build did not include the commit.
		{
			commit:        details[hashes['D']],
			build:         build1,
			expectedScore: -0.9690254634399716,
			lambda:        lambda(0.5),
		},
		// Build is nil.
		{
			commit:        details[hashes['A']],
			build:         nil,
			expectedScore: -0.958902488117383,
			lambda:        lambda(0.5),
		},
		// Same cases, but with an even more agressive lambda.
		// Built at the given commit.
		{
			commit:        details[hashes['A']],
			build:         build1,
			expectedScore: 0.756679619938755,
			lambda:        lambda(0.01),
		},
		// Build included the commit.
		{
			commit:        details[hashes['B']],
			build:         build1,
			expectedScore: 0.269316526502904,
			lambda:        lambda(0.01),
		},
		// Build included the commit.
		{
			commit:        details[hashes['C']],
			build:         build1,
			expectedScore: 0.2703808739655321,
			lambda:        lambda(0.01),
		},
		// Build did not include the commit.
		{
			commit:        details[hashes['D']],
			build:         build1,
			expectedScore: -0.8113588225688924,
			lambda:        lambda(0.01),
		},
		// Build is nil.
		{
			commit:        details[hashes['A']],
			build:         nil,
			expectedScore: -0.756679619938755,
			lambda:        lambda(0.01),
		},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.expectedScore, scoreBuild(tc.commit, tc.build, now, tc.lambda))
	}
}

type buildQueueExpect struct {
	bc  *BuildCandidate
	err error
}

func testBuildQueue(t *testing.T, timeDecay24Hr float64, getExpectations func(*repograph.Graph) []*buildQueueExpect, testInsert bool) {
	testutils.MediumTest(t)
	testutils.SkipIfShort(t)

	// Initialize the buildbot database.
	d := clearDB(t)
	defer d.Close(t)

	// Load the test repo.
	tr := util.NewTempRepo()
	defer tr.Cleanup()

	remote := path.Join(tr.Dir, "skia.git")
	repo, err := repograph.NewGraph(remote, tr.Dir)
	assert.NoError(t, err)
	repos := repograph.Map{
		common.REPO_SKIA: repo,
	}

	// Insert an initial build.
	buildNum := 0
	b := &buildbot.Build{
		Builder:     TEST_BUILDER,
		Master:      "fake",
		Number:      buildNum,
		BuildSlave:  "fake",
		Branch:      "master",
		GotRevision: hashes['A'],
		Repository:  TEST_REPO,
		Started:     time.Now(),
	}
	assert.NoError(t, buildbot.IngestBuild(d.db, b, repos))
	buildNum++

	// Create the BuildQueue.
	tmp, err := ioutil.TempDir("", "")
	assert.NoError(t, err)
	defer testutils.RemoveAll(t, tmp)
	bl, err := blacklist.FromFile(path.Join(tmp, "blacklist.json"))
	assert.NoError(t, err)
	q, err := NewBuildQueue(PERIOD_FOREVER, repos, DEFAULT_SCORE_THRESHOLD, timeDecay24Hr, bl, d.db)
	assert.NoError(t, err)

	// Fake time.Now()
	details := repo.Get(hashes['I'])
	assert.NotNil(t, details)
	now := details.Timestamp.Add(1 * time.Hour)

	// Update the queue.
	assert.NoError(t, q.update(now))

	// Ensure that we get the expected BuildCandidate at each step. Insert
	// each BuildCandidate into the buildbot database to simulate actually
	// running builds.
	for _, expected := range getExpectations(repo) {
		bc, err := q.Pop([]string{TEST_BUILDER})
		assert.Equal(t, expected.err, err)
		if err != nil {
			break
		}
		testutils.AssertDeepEqual(t, expected.bc, bc)
		if testInsert || buildNum == 0 {
			// Actually insert a build, as if we're really using the scheduler.
			// Do this even if we're not testing insertion, because if we don't,
			// the queue won't know about this builder.
			b := &buildbot.Build{
				Builder:     bc.Builder,
				Master:      "fake",
				Number:      buildNum,
				BuildSlave:  "fake",
				Branch:      "master",
				GotRevision: bc.Commit.Hash,
				Repository:  TEST_REPO,
				Started:     time.Now(),
			}
			assert.NoError(t, buildbot.IngestBuild(d.db, b, repos))
			buildNum++
			assert.NoError(t, q.update(now))
		}
	}
}

func zeroLambdaExpectations(r *repograph.Graph) []*buildQueueExpect {
	return []*buildQueueExpect{
		// First round: a single build at origin/master.
		{
			&BuildCandidate{
				Commit:  r.Get(hashes['I']),
				Builder: TEST_BUILDER,
				Score:   9.875,
				Repo:    TEST_REPO,
			},
			nil,
		},
		// Second round: bisect 8 -> 4 + 4
		{
			&BuildCandidate{
				Commit:  r.Get(hashes['E']),
				Builder: TEST_BUILDER,
				Score:   1.625,
				Repo:    TEST_REPO,
			},
			nil,
		},
		// Third round: bisect 4 + 4 -> 4 + 2 + 2
		{
			&BuildCandidate{
				Commit:  r.Get(hashes['C']),
				Builder: TEST_BUILDER,
				Score:   1.25,
				Repo:    TEST_REPO,
			},
			nil,
		},
		// Fourth round: bisect 4 + 2 + 2 -> 2 + 2 + 2 + 2
		{
			&BuildCandidate{
				Commit:  r.Get(hashes['H']),
				Builder: TEST_BUILDER,
				Score:   1.25,
				Repo:    TEST_REPO,
			},
			nil,
		},
		// Fifth round: bisect 2 + 2 + 2 + 2 -> 2 + 2 + 2 + 1 + 1
		{
			&BuildCandidate{
				Commit:  r.Get(hashes['F']),
				Builder: TEST_BUILDER,
				Score:   0.5,
				Repo:    TEST_REPO,
			},
			nil,
		},
		// Sixth round: bisect 2 + 2 + 2 + 1 + 1 -> 2 + 2 + 1 + 1 + 1 + 1
		{
			&BuildCandidate{
				Commit:  r.Get(hashes['G']),
				Builder: TEST_BUILDER,
				Score:   0.5,
				Repo:    TEST_REPO,
			},
			nil,
		},
		// Seventh round: bisect 2 + 2 + 1 + 1 + 1 + 1 -> 2 + 1 + 1 + 1 + 1 + 1 + 1
		{
			&BuildCandidate{
				Commit:  r.Get(hashes['D']),
				Builder: TEST_BUILDER,
				Score:   0.5,
				Repo:    TEST_REPO,
			},
			nil,
		},
		// Eighth round: bisect 2 + 1 + 1 + 1 + 1 + 1 + 1 -> 1 + 1 + 1 + 1 + 1 + 1 + 1 + 1
		{
			&BuildCandidate{
				Commit:  r.Get(hashes['B']),
				Builder: TEST_BUILDER,
				Score:   0.5,
				Repo:    TEST_REPO,
			},
			nil,
		},
		// Ninth round: All commits individually tested; Score is 0.
		{
			nil,
			ERR_EMPTY_QUEUE,
		},
	}
}

func TestBuildQueueZeroLambdaNoInsert(t *testing.T) {
	testBuildQueue(t, 1.0, zeroLambdaExpectations, false)
}

func TestBuildQueueZeroLambdaInsert(t *testing.T) {
	testBuildQueue(t, 1.0, zeroLambdaExpectations, true)
}

func lambdaExpectations(r *repograph.Graph) []*buildQueueExpect {
	return []*buildQueueExpect{
		// First round: a single build at origin/master.
		{
			&BuildCandidate{
				Commit:  r.Get(hashes['I']),
				Builder: TEST_BUILDER,
				Score:   9.199112062778687,
				Repo:    TEST_REPO,
			},
			nil,
		},
		// Second round: bisect 8 -> 4 + 8
		{
			&BuildCandidate{
				Commit:  r.Get(hashes['E']),
				Builder: TEST_BUILDER,
				Score:   1.5115399445789144,
				Repo:    TEST_REPO,
			},
			nil,
		},
		// Third round: bisect 4 + 4 -> 2 + 2 + 4
		{
			&BuildCandidate{
				Commit:  r.Get(hashes['H']),
				Builder: TEST_BUILDER,
				Score:   1.1659240823148886,
				Repo:    TEST_REPO,
			},
			nil,
		},
		// Fourth round: bisect 2 + 2 + 4 -> 2 + 2 + 2 + 2
		{
			&BuildCandidate{
				Commit:  r.Get(hashes['C']),
				Builder: TEST_BUILDER,
				Score:   1.1615269290297145,
				Repo:    TEST_REPO,
			},
			nil,
		},
		// Fifth round: bisect 2 + 2 + 2 + 2 -> 1 + 1 + 2 + 2 + 2
		{
			&BuildCandidate{
				Commit:  r.Get(hashes['F']),
				Builder: TEST_BUILDER,
				Score:   0.46716910846026205,
				Repo:    TEST_REPO,
			},
			nil,
		},
		// Sixth round: bisect 1 + 1 + 2 + 2 + 2 -> 1 + 1 + 1 + 1 + 2 + 2
		{
			&BuildCandidate{
				Commit:  r.Get(hashes['G']),
				Builder: TEST_BUILDER,
				Score:   0.46518052365376716,
				Repo:    TEST_REPO,
			},
			nil,
		},
		// Seventh round: bisect 1 + 1 + 1 + 1 + 2 + 2 -> 1 + 1 + 1 + 1 + 1 + 1 + 2
		{
			&BuildCandidate{
				Commit:  r.Get(hashes['D']),
				Builder: TEST_BUILDER,
				Score:   0.464773434279506,
				Repo:    TEST_REPO,
			},
			nil,
		},
		// Eighth round: bisect 1 + 1 + 1 + 1 + 1 + 1 + 2 -> 1 + 1 + 1 + 1 + 1 + 1 + 1 + 1
		{
			&BuildCandidate{
				Commit:  r.Get(hashes['B']),
				Builder: TEST_BUILDER,
				Score:   0.4640899801691636,
				Repo:    TEST_REPO,
			},
			nil,
		},
		// Ninth round: All commits individually tested; Score is 0.
		{
			nil,
			ERR_EMPTY_QUEUE,
		},
	}
}

func TestBuildQueueLambdaNoInsert(t *testing.T) {
	testBuildQueue(t, 0.2, lambdaExpectations, false)
}

func TestBuildQueueLambdaInsert(t *testing.T) {
	testBuildQueue(t, 0.2, lambdaExpectations, true)
}

func TestBuildQueueNoPrevious(t *testing.T) {
	testutils.MediumTest(t)
	testutils.SkipIfShort(t)

	// Initialize the buildbot database.
	d := clearDB(t)
	defer d.Close(t)

	// Load the test repo.
	tr := util.NewTempRepo()
	defer tr.Cleanup()

	remote := path.Join(tr.Dir, "skia.git")
	repo, err := repograph.NewGraph(remote, tr.Dir)
	assert.NoError(t, err)
	repos := repograph.Map{
		common.REPO_SKIA: repo,
	}

	// Create the BuildQueue.
	tmp, err := ioutil.TempDir("", "")
	assert.NoError(t, err)
	defer testutils.RemoveAll(t, tmp)
	bl, err := blacklist.FromFile(path.Join(tmp, "blacklist.json"))
	assert.NoError(t, err)
	q, err := NewBuildQueue(PERIOD_FOREVER, repos, DEFAULT_SCORE_THRESHOLD, 1.0, bl, d.db)
	assert.NoError(t, err)

	// Fake time.Now()
	details := repo.Get(hashes['I'])
	assert.NotNil(t, details)
	now := details.Timestamp.Add(1 * time.Hour)

	// Update the queue.
	assert.NoError(t, q.update(now))

	// Make sure we get the right candidate: when there are no previous
	// builds, we should schedule a build at origin/master with the maximum
	// score.
	bc, err := q.Pop([]string{TEST_BUILDER})
	assert.NoError(t, err)
	assert.Equal(t, &BuildCandidate{
		Commit:  repo.Get(hashes['I']),
		Builder: TEST_BUILDER,
		Score:   math.MaxFloat64,
		Repo:    TEST_REPO,
	}, bc)
}

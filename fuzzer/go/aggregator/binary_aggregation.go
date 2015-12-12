package aggregator

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	go_metrics "github.com/rcrowley/go-metrics"
	"github.com/skia-dev/glog"
	"go.skia.org/infra/fuzzer/go/common"
	"go.skia.org/infra/fuzzer/go/config"
	"go.skia.org/infra/go/exec"
	"go.skia.org/infra/go/fileutil"
	"go.skia.org/infra/go/util"
	"golang.org/x/net/context"
	"google.golang.org/cloud/storage"
)

var storageClient *storage.Client

// AnalysisPackage is a generic holder for the functions needed to analyze
type AnalysisPackage struct {
	Setup   func(workingDirPath string) error
	Analyze func(workingDirPath, pathToFile string) (uploadPackage, error)
}

// uploadPackage is a struct containing all the pieces of a bad fuzz that need to be uploaded to GCS
type uploadPackage struct {
	Name        string
	FilePath    string
	DebugDump   string
	DebugErr    string
	ReleaseDump string
	ReleaseErr  string
	Type        string
}

// StartBinaryAggregator will find new bad binary fuzzes generated by afl-fuzz and create the
// metadata required for them.
// It does this by searching in the specified AflOutputPath for new crashes and moves them to a
// temporary holding folder (specified by BinaryFuzzPath) for parsing, before uploading them to GCS
func StartBinaryAggregator(s *storage.Client) error {
	storageClient = s
	if _, err := fileutil.EnsureDirExists(config.Aggregator.BinaryFuzzPath); err != nil {
		return err
	}
	if _, err := fileutil.EnsureDirExists(config.Aggregator.ExecutablePath); err != nil {
		return err
	}
	if err := common.BuildClangDM("Debug", true); err != nil {
		return err
	}
	if err := common.BuildClangDM("Release", true); err != nil {
		return err
	}

	// For passing the paths of new binaries that should be scanned.
	forAnalysis := make(chan string, 10000)
	// For passing the file names of analyzed fuzzes that should be uploaded from where they rest on
	// disk in config.Aggregator.BinaryFuzzPath
	forUpload := make(chan uploadPackage, 100)
	// For passing the names of go routines that had to stop.  If the aggregation process fails,
	// everything else will be killed.
	terminated := make(chan string)
	go scanForNewCandidates(forAnalysis, terminated)

	numAnalysisProcesses := config.Aggregator.NumAnalysisProcesses
	if numAnalysisProcesses <= 0 {
		// TODO(kjlubick): Actually make this smart based on the number of cores
		numAnalysisProcesses = 20
	}
	for i := 0; i < numAnalysisProcesses; i++ {
		go performAnalysis(i, analyzeSkp, forAnalysis, forUpload, terminated)
	}

	numUploadProcesses := config.Aggregator.NumUploadProcesses
	if numUploadProcesses <= 0 {
		// TODO(kjlubick): Actually make this smart based on the number of cores/number
		// of aggregation processes
		numUploadProcesses = 5
	}
	for i := 0; i < numUploadProcesses; i++ {
		go waitForUploads(i, forUpload, terminated)
	}

	analysisProcessCount := go_metrics.NewRegisteredCounter("analysis_process_count", go_metrics.DefaultRegistry)
	analysisProcessCount.Inc(int64(numAnalysisProcesses))
	uploadProcessCount := go_metrics.NewRegisteredCounter("upload_process_count", go_metrics.DefaultRegistry)
	uploadProcessCount.Inc(int64(numUploadProcesses))

	t := time.Tick(config.Aggregator.StatusPeriod)
	for {
		select {
		case _ = <-t:
			go_metrics.GetOrRegisterGauge("binary_analysis_queue_size", go_metrics.DefaultRegistry).Update(int64(len(forAnalysis)))
			go_metrics.GetOrRegisterGauge("binary_upload_queue_size", go_metrics.DefaultRegistry).Update(int64(len(forUpload)))
		case deadService := <-terminated:
			glog.Errorf("%s died", deadService)
			if deadService == "scanner" {
				return fmt.Errorf("Ending aggregator: The afl-fuzz scanner died.")
			} else if strings.HasPrefix(deadService, "analyzer") {
				if analysisProcessCount.Dec(1); analysisProcessCount.Count() <= 0 {
					return fmt.Errorf("Ending aggregator: No more analysis processes alive")
				}
			} else if strings.HasPrefix(deadService, "uploader") {
				if uploadProcessCount.Dec(1); uploadProcessCount.Count() <= 0 {
					return fmt.Errorf("Ending aggregator: No more upload processes alive")
				}
			}
		}
	}
}

// scanForNewCandidates runs scanHelper once every config.Aggregator.RescanPeriod, which scans the
// config.Generator.AflOutputPath for new fuzzes.
// If scanHelper returns an error, this method will terminate.
func scanForNewCandidates(forAnalysis, terminated chan<- string) {
	// Logs an error and writes to the terminated channel.
	prepareForExit := func(err error) {
		glog.Errorf("Scanner terminated due to error: %v", err)
		terminated <- "scanner"
	}
	alreadyFoundBinaries := &SortedStringSlice{}
	// time.Tick does not fire immediately, so we fire it manually once.
	if err := scanHelper(alreadyFoundBinaries, forAnalysis); err != nil {
		prepareForExit(err)
		return
	}
	glog.Infof("Sleeping for %s, then waking up to find new crashes again", config.Aggregator.RescanPeriod)

	for _ = range time.Tick(config.Aggregator.RescanPeriod) {
		if err := scanHelper(alreadyFoundBinaries, forAnalysis); err != nil {
			prepareForExit(err)
			return
		}
		glog.Infof("Sleeping for %s, then waking up to find new crashes again", config.Aggregator.RescanPeriod)
	}
}

// scanHelper runs findBadBinaryPaths, logs the output and keeps alreadyFoundBinaries up to date.
func scanHelper(alreadyFoundBinaries *SortedStringSlice, forAnalysis chan<- string) error {
	newlyFound, err := findBadBinaryPaths(alreadyFoundBinaries)
	if err != nil {
		return err
	}
	// AFL-fuzz does not write crashes or hangs atomically, so this workaround waits for a bit after
	// we have references to where the crashes will be.
	// TODO(kjlubick), switch to using flock once afl-fuzz implements that upstream.
	time.Sleep(time.Second)
	go_metrics.GetOrRegisterGauge("binary_newly_found_fuzzes", go_metrics.DefaultRegistry).Update(int64(len(newlyFound)))
	glog.Infof("%d newly found bad binary fuzzes", len(newlyFound))
	for _, f := range newlyFound {
		forAnalysis <- f
	}
	alreadyFoundBinaries.Append(newlyFound)
	return nil
}

// findBadBinaryPaths looks through all the afl-fuzz directories contained in the passed in path and
// returns the path to all files that are in a crash* folder that are not already in
// 'alreadyFoundBinaries'
// It also sends them to the forAnalysis channel when it finds them.
// The output from afl-fuzz looks like:
// $AFL_ROOT/
//		-fuzzer0/
//			-crashes/  <-- bad binary fuzzes end up here
//			-hangs/
//			-queue/
//			-fuzzer_stats
//		-fuzzer1/
//		...
func findBadBinaryPaths(alreadyFoundBinaries *SortedStringSlice) ([]string, error) {
	badBinaryPaths := make([]string, 0)

	aflDir, err := os.Open(config.Generator.AflOutputPath)
	if err != nil {
		return nil, err
	}
	defer util.Close(aflDir)

	fuzzerFolders, err := aflDir.Readdir(-1)
	if err != nil {
		return nil, err
	}

	for _, fuzzerFolderInfo := range fuzzerFolders {
		// fuzzerFolderName an os.FileInfo like fuzzer0, fuzzer1
		path := filepath.Join(config.Generator.AflOutputPath, fuzzerFolderInfo.Name())
		fuzzerDir, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer util.Close(fuzzerDir)

		fuzzerContents, err := fuzzerDir.Readdir(-1)
		if err != nil {
			return nil, err
		}
		for _, info := range fuzzerContents {
			// Look through fuzzerN/crashes
			if info.IsDir() && strings.HasPrefix(info.Name(), "crashes") {
				crashPath := filepath.Join(path, info.Name())
				crashDir, err := os.Open(crashPath)
				if err != nil {
					return nil, err
				}
				defer util.Close(crashDir)

				crashContents, err := crashDir.Readdir(-1)
				if err != nil {
					return nil, err
				}
				for _, crash := range crashContents {
					// Make sure the files are actually crashable files we haven't found before
					if crash.Name() != "README.txt" {
						if fuzzPath := filepath.Join(crashPath, crash.Name()); !alreadyFoundBinaries.Contains(fuzzPath) {
							badBinaryPaths = append(badBinaryPaths, fuzzPath)
						}
					}
				}
			}
		}
	}
	return badBinaryPaths, nil
}

// performAnalysis waits for files that need to be analyzed (from forAnalysis) and makes a copy of
// them in config.Aggregator.BinaryFuzzPath with their hash as a file name.
// It then analyzes it using the supplied AnalysisPackage and then signals the results should be
// uploaded. If any unrecoverable errors happen, this method terminates.
func performAnalysis(identifier int, analysisPackage AnalysisPackage, forAnalysis <-chan string, forUpload chan<- uploadPackage, terminated chan<- string) {
	glog.Infof("Spawning analyzer %d", identifier)
	prepareForExit := func(err error) {
		glog.Errorf("Analyzer %d terminated due to error: %s", identifier, err)
		terminated <- fmt.Sprintf("analyzer%d", identifier)
	}
	// our own unique working folder
	executableDir := filepath.Join(config.Aggregator.ExecutablePath, fmt.Sprintf("analyzer%d", identifier))

	if err := analysisPackage.Setup(executableDir); err != nil {
		prepareForExit(err)
		return
	}

	for {
		badBinaryPath := <-forAnalysis
		hash, data, err := calculateHash(badBinaryPath)
		if err != nil {
			prepareForExit(err)
			return
		}
		newFuzzPath := filepath.Join(config.Aggregator.BinaryFuzzPath, hash)
		if err := ioutil.WriteFile(newFuzzPath, data, 0644); err != nil {
			prepareForExit(err)
			return
		}
		if upload, err := analysisPackage.Analyze(executableDir, hash); err != nil {
			glog.Errorf("Problem analyzing %s", newFuzzPath)
			prepareForExit(err)
			return
		} else {
			forUpload <- upload
		}
	}
}

// analyzeSkp is an analysisPackage for analyzing skp files.
// Setup cleans out the work space, makes a copy of the Debug and Release parseskp executable.
// Analyze simply invokes performBinaryAnalysis using parse_skp on the files that are passed in.
var analyzeSkp = AnalysisPackage{
	Setup: func(workingDirPath string) error {
		// Delete all previous binaries to get a clean start
		if err := os.RemoveAll(workingDirPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(workingDirPath, 0755); err != nil {
			return err
		}

		// make a copy of the debug and release executables
		basePath := filepath.Join(config.Generator.SkiaRoot, "out")
		if err := fileutil.CopyExecutable(filepath.Join(basePath, "Debug", common.TEST_HARNESS_NAME), filepath.Join(workingDirPath, common.TEST_HARNESS_NAME+"_debug")); err != nil {
			return err
		}
		if err := fileutil.CopyExecutable(filepath.Join(basePath, "Release", common.TEST_HARNESS_NAME), filepath.Join(workingDirPath, common.TEST_HARNESS_NAME+"_release")); err != nil {
			return err
		}

		return nil
	},
	Analyze: func(workingDirPath, skpFileName string) (uploadPackage, error) {
		upload := uploadPackage{
			Name:     skpFileName,
			Type:     "skp",
			FilePath: filepath.Join(config.Aggregator.BinaryFuzzPath, skpFileName),
		}

		if dump, stderr, err := performBinaryAnalysis(workingDirPath, common.TEST_HARNESS_NAME, skpFileName, true); err != nil {
			return upload, err
		} else {
			upload.DebugDump = dump
			upload.DebugErr = stderr
		}
		if dump, stderr, err := performBinaryAnalysis(workingDirPath, common.TEST_HARNESS_NAME, skpFileName, false); err != nil {
			return upload, err
		} else {
			upload.ReleaseDump = dump
			upload.ReleaseErr = stderr
		}
		return upload, nil
	},
}

// performBinaryAnalysis executes a command like:
// timeout AnalysisTimeout catchsegv ./parse_foo_debug --input badbeef
// from the working dir specified.
// GNU timeout is used instead of the option on exec.Command because experimentation with the latter
// showed evidence of that way leaking processes, which lead to OOM errors.
// GNU catchsegv generates human readable dumps of crashes, which can then be scanned for stacktrace
// information. The dumps (which come via standard out) and standard errors are recorded as strings.
func performBinaryAnalysis(workingDirPath, baseExecutableName, fileName string, isDebug bool) (string, string, error) {
	suffix := "_release"
	if isDebug {
		suffix = "_debug"
	}

	pathToFile := filepath.Join(config.Aggregator.BinaryFuzzPath, fileName)
	pathToExecutable := fmt.Sprintf("./%s%s", baseExecutableName, suffix)
	timeoutInSeconds := fmt.Sprintf("%ds", config.Aggregator.AnalysisTimeout/time.Second)

	var dump bytes.Buffer
	var stdErr bytes.Buffer

	cmd := &exec.Command{
		Name:      "timeout",
		Args:      []string{timeoutInSeconds, "catchsegv", pathToExecutable, "--src", "skp", "--skps", pathToFile, "--config", "8888"},
		LogStdout: false,
		LogStderr: false,
		Stdout:    &dump,
		Stderr:    &stdErr,
		Dir:       workingDirPath,
	}

	//errors are fine/expected from this, as we are dealing with bad fuzzes
	if err := exec.Run(cmd); err != nil {
		return dump.String(), stdErr.String(), nil
	}
	return dump.String(), stdErr.String(), nil
}

// calcuateHash calculates the sha1 hash of a file, given its path.  It returns both the hash as a
// hex-encoded string and the contents of the file.
func calculateHash(path string) (hash string, data []byte, err error) {
	data, err = ioutil.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("Problem reading file for hashing %s: %s", path, err)
	}
	return fmt.Sprintf("%x", sha1.Sum(data)), data, nil
}

// A SortedStringSlice has a sortable string slice which is always kept sorted.
// This allows for an implementation of Contains that runs in O(log n)
type SortedStringSlice struct {
	strings sort.StringSlice
}

// Contains returns true if the passed in string is in the underlying slice
func (s *SortedStringSlice) Contains(str string) bool {
	i := s.strings.Search(str)
	if i < len(s.strings) && s.strings[i] == str {
		return true
	}
	return false
}

// Append adds all of the strings to the underlying slice and sorts it
func (s *SortedStringSlice) Append(strs []string) {
	s.strings = append(s.strings, strs...)
	s.strings.Sort()
}

// waitForUploads waits for uploadPackages to be sent through the forUpload channel
// and then uploads them.  If any unrecoverable errors happen, this method terminates.
func waitForUploads(identifier int, forUpload <-chan uploadPackage, terminated chan<- string) {
	glog.Infof("Spawning uploader %d", identifier)
	for {
		p := <-forUpload
		if err := upload(p); err != nil {
			glog.Errorf("Uploader %d terminated due to error: %s", identifier, err)
			terminated <- fmt.Sprintf("uploader%d", identifier)
			return
		}
	}
}

// upload breaks apart the uploadPackage into its constituant parts and uploads them to
// GCS using some helper methods.
func upload(p uploadPackage) error {
	glog.Infof("uploading %s with file %s and analysis bytes %d;%d;%d;%d ", p.Name, p.FilePath, len(p.DebugDump), len(p.DebugErr), len(p.ReleaseDump), len(p.ReleaseErr))

	if err := uploadBinaryFromDisk(p.Type, p.Name, p.Name, p.FilePath); err != nil {
		return err
	}
	if err := uploadString(p.Type, p.Name, p.Name+"_debug.dump", p.DebugDump); err != nil {
		return err
	}
	if err := uploadString(p.Type, p.Name, p.Name+"_debug.err", p.DebugErr); err != nil {
		return err
	}
	if err := uploadString(p.Type, p.Name, p.Name+"_release.dump", p.ReleaseDump); err != nil {
		return err
	}
	return uploadString(p.Type, p.Name, p.Name+"_release.err", p.ReleaseErr)
}

// uploadBinaryFromDisk uploads a binary file on disk to GCS, returning an error if
// anything goes wrong.
func uploadBinaryFromDisk(fuzzType, fuzzName, fileName, filePath string) error {
	name := fmt.Sprintf("binary_fuzzes/bad/%s/%s/%s", fuzzType, fuzzName, fileName)
	w := storageClient.Bucket(config.GS.Bucket).Object(name).NewWriter(context.Background())
	defer util.Close(w)
	// We set the encoding to avoid accidental crashes if Chrome were to try to render
	// a fuzzed png or svg or something.
	w.ObjectAttrs.ContentEncoding = "application/octet-stream"

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("There was a problem reading %s for uploading : %s", filePath, err)
	}

	if n, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("There was a problem uploading binary %s.  Only uploaded %d bytes : %s", name, n, err)
	}
	return nil
}

// uploadBinaryFromDisk uploads the contents of a string as a file to GCS, returning an error if
// anything goes wrong.
func uploadString(fuzzType, fuzzName, fileName, contents string) error {
	name := fmt.Sprintf("binary_fuzzes/bad/%s/%s/%s", fuzzType, fuzzName, fileName)
	w := storageClient.Bucket(config.GS.Bucket).Object(name).NewWriter(context.Background())
	defer util.Close(w)
	w.ObjectAttrs.ContentEncoding = "text/plain"

	if n, err := w.Write([]byte(contents)); err != nil {
		return fmt.Errorf("There was a problem uploading %s.  Only uploaded %d bytes: %s", name, n, err)
	}
	return nil
}

package main

/*
Runs the backend portions of the fuzzer.  This includes the generator and aggregator parts (see DESIGN.md)
*/

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/skia-dev/glog"
	"go.skia.org/infra/fuzzer/go/aggregator"
	fcommon "go.skia.org/infra/fuzzer/go/common"
	"go.skia.org/infra/fuzzer/go/config"
	"go.skia.org/infra/fuzzer/go/generator"
	"go.skia.org/infra/go/auth"
	"go.skia.org/infra/go/common"
	"go.skia.org/infra/go/fileutil"
	"golang.org/x/net/context"
	"google.golang.org/cloud"
	"google.golang.org/cloud/storage"
)

var (
	aflOutputPath      = flag.String("afl_output_path", "", "[REQUIRED] The output folder of afl-fuzz.  This will have N folders named fuzzer0 - fuzzerN.  Should not be in /tmp or afl-fuzz will refuse to run.")
	generatorWD        = flag.String("generator_working_dir", "", "[REQUIRED] The generator's working directory.  Should not be in /tmp.")
	fuzzSamples        = flag.String("fuzz_samples", "", "[REQUIRED] The generator's working directory.  Should not be in /tmp.")
	skiaRoot           = flag.String("skia_root", "", "[REQUIRED] The root directory of the Skia source code.")
	clangPath          = flag.String("clang_path", "", "[REQUIRED] The path to the clang executable.")
	clangPlusPlusPath  = flag.String("clang_p_p_path", "", "[REQUIRED] The path to the clang++ executable.")
	depotToolsPath     = flag.String("depot_tools_path", "", "The absolute path to depot_tools.  Can be empty if they are on your path.")
	aflRoot            = flag.String("afl_root", "", "[REQUIRED] The install directory of afl-fuzz (v1.94b or later).")
	numFuzzProcesses   = flag.Int("fuzz_processes", 0, `The number of processes to run afl-fuzz.  This should be fewer than the number of logical cores.  Defaults to 0, which means "Make an intelligent guess"`)
	watchAFL           = flag.Bool("watch_afl", false, "(debug only) If the afl master's output should be piped to stdout.")
	versionCheckPeriod = flag.Duration("version_check_period", 20*time.Second, `The period used to check the version of Skia that needs fuzzing.`)
	downloadProcesses  = flag.Int("download_processes", 4, "The number of download processes to be used for fetching fuzzes.")

	bucket               = flag.String("bucket", "skia-fuzzer", "The GCS bucket in which to store found fuzzes.")
	binaryFuzzPath       = flag.String("fuzz_path", filepath.Join(os.TempDir(), "fuzzes"), "The directory to temporarily store the binary fuzzes during aggregation.")
	executablePath       = flag.String("executable_path", filepath.Join(os.TempDir(), "executables"), "The directory to store temporary executables that will run the fuzzes during aggregation. Defaults to /tmp/executables.")
	numAnalysisProcesses = flag.Int("analysis_processes", 0, `The number of processes to analyze fuzzes.  This should be fewer than the number of logical cores.  Defaults to 0, which means "Make an intelligent guess"`)
	rescanPeriod         = flag.Duration("rescan_period", 60*time.Second, `The time in which to sleep for every cycle of aggregation. `)
	numUploadProcesses   = flag.Int("upload_processes", 0, `The number of processes to upload fuzzes. Defaults to 0, which means "Make an intelligent guess"`)
	statusPeriod         = flag.Duration("status_period", 60*time.Second, `The time period used to report the status of the aggregation/analysis/upload queue. `)
	analysisTimeout      = flag.Duration("analysis_timeout", 5*time.Second, `The maximum time an analysis should run.`)

	graphiteServer = flag.String("graphite_server", "localhost:2003", "Where is Graphite metrics ingestion server running.")
)

var (
	requiredFlags                 = []string{"afl_output_path", "skia_root", "clang_path", "clang_p_p_path", "afl_root", "generator_working_dir"}
	storageClient *storage.Client = nil
)

func main() {
	defer common.LogPanic()
	// Calls flag.Parse()
	common.InitWithMetrics("fuzzer-be", graphiteServer)

	if err := writeFlagsToConfig(); err != nil {
		glog.Fatalf("Problem with configuration: %v", err)
	}
	if err := setupOAuth(); err != nil {
		glog.Fatalf("Problem with OAuth: %s", err)
	}
	if err := fcommon.DownloadSkiaVersionForFuzzing(storageClient, config.Generator.SkiaRoot, &config.Generator); err != nil {
		glog.Fatalf("Problem downloading Skia: %s", err)
	}
	if err := generator.DownloadBinarySeedFiles(storageClient); err != nil {
		glog.Fatalf("Problem downloading binary seed files: %s", err)
	}

	glog.Infof("Starting generator with configuration %#v", config.Generator)
	if err := generator.StartBinaryGenerator(); err != nil {
		glog.Fatalf("Problem starting binary generator: %s", err)
	}

	glog.Infof("Starting aggregator with configuration %#v", config.Aggregator)
	agg, err := aggregator.StartBinaryAggregator(storageClient)
	if err != nil {
		glog.Fatalf("Could not start aggregator: %s", err)
	}

	glog.Info("Starting version watcher")
	status := generator.StartVersionWatcher(storageClient, agg)

	err = <-status
	glog.Fatal(err)
}

func writeFlagsToConfig() error {
	// Check the required ones and terminate if they are not provided
	for _, f := range requiredFlags {
		if flag.Lookup(f).Value.String() == "" {
			return fmt.Errorf("Required flag %s is empty.", f)
		}
	}
	var err error
	config.Generator.AflOutputPath, err = fileutil.EnsureDirExists(*aflOutputPath)
	if err != nil {
		return err
	}
	config.Generator.SkiaRoot, err = fileutil.EnsureDirExists(*skiaRoot)
	if err != nil {
		return err
	}
	config.Generator.AflRoot, err = fileutil.EnsureDirExists(*aflRoot)
	if err != nil {
		return err
	}
	config.Generator.WorkingPath, err = fileutil.EnsureDirExists(*generatorWD)
	if err != nil {
		return err
	}
	config.Generator.FuzzSamples, err = fileutil.EnsureDirExists(*fuzzSamples)
	if err != nil {
		return err
	}

	config.Common.ClangPath = *clangPath
	config.Common.ClangPlusPlusPath = *clangPlusPlusPath
	config.Common.DepotToolsPath = *depotToolsPath
	config.Generator.NumFuzzProcesses = *numFuzzProcesses
	config.Generator.WatchAFL = *watchAFL
	config.Generator.VersionCheckPeriod = *versionCheckPeriod
	config.Generator.NumDownloadProcesses = *downloadProcesses

	config.GS.Bucket = *bucket
	config.Aggregator.BinaryFuzzPath, err = fileutil.EnsureDirExists(*binaryFuzzPath)
	if err != nil {
		return err
	}
	config.Aggregator.ExecutablePath, err = fileutil.EnsureDirExists(*executablePath)
	if err != nil {
		return err
	}
	config.Aggregator.NumAnalysisProcesses = *numAnalysisProcesses
	config.Aggregator.NumUploadProcesses = *numUploadProcesses
	config.Aggregator.StatusPeriod = *statusPeriod
	config.Aggregator.RescanPeriod = *rescanPeriod
	config.Aggregator.AnalysisTimeout = *analysisTimeout
	return nil
}

func setupOAuth() error {
	client, err := auth.NewDefaultJWTServiceAccountClient(auth.SCOPE_READ_WRITE)
	if err != nil {
		return fmt.Errorf("Problem setting up client OAuth: %v", err)
	}

	if storageClient, err = storage.NewClient(context.Background(), cloud.WithBaseHTTP(client)); err != nil {
		return fmt.Errorf("Problem authenticating: %v", err)
	}
	return nil
}

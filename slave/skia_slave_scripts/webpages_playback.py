#!/usr/bin/env python
# Copyright (c) 2012 The Chromium Authors. All rights reserved.
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

"""Archives or replays webpages and creates skps in a Google Storage location.

To archive webpages and store skp files (will be run rarely):

cd ../buildbot/slave/skia_slave_scripts
python webpages_playback.py --dest_gsbase=gs://rmistry \
--record=True


To replay archived webpages and re-generate skp files (will be run whenever
SkPicture.PICTURE_VERSION changes):

cd ../buildbot/slave/skia_slave_scripts
python webpages_playback.py --dest_gsbase=gs://rmistry

Specify the --page_sets flag (default value is 'all') to pick a list of which
webpages should be archived and/or replayed. Eg:

cd ../buildbot/slave/skia_slave_scripts
python webpages_playback.py --dest_gsbase=gs://rmistry \
--page_sets=page_sets/skia_yahooanswers_desktop.json,\
page_sets/skia_wikipedia_galaxynexus.json

The --do_not_upload_to_gs=True flag will not upload to Google Storage (default
value is 'False').

The --debugger flag if specified will allow you to preview the captured skp
before proceeding to the next step. It needs to point to the built debugger. Eg:
trunk/out/Debug/debugger

"""

import cPickle
import glob
import json
import optparse
import os
import posixpath
import shutil
import sys
import tempfile
import time
import traceback
import urllib2


# Set the PYTHONPATH for this script to include chromium_buildbot scripts,
# site_config, perf, telemetry and webpagereplay.
sys.path.append(os.path.join(os.pardir, os.pardir, 'tools'))
sys.path.append(
    os.path.join(os.pardir, os.pardir, 'third_party', 'chromium_trunk',
                 'chrome', 'test', 'functional'))
sys.path.append(
    os.path.join(os.pardir, os.pardir, 'third_party', 'chromium_trunk',
                 'tools', 'perf'))
sys.path.append(
    os.path.join(os.pardir, os.pardir, 'third_party', 'chromium_trunk',
                 'tools', 'telemetry'))
sys.path.append(
    os.path.join(os.pardir, os.pardir, 'third_party', 'src', 'third_party',
                 'webpagereplay'))
sys.path.append(
    os.path.join(os.pardir, os.pardir, 'third_party', 'chromium_buildbot',
                 'scripts'))
sys.path.append(
    os.path.join(os.pardir, os.pardir, 'third_party', 'chromium_buildbot',
                 'site_config'))

from perf_tools import skpicture_printer
from slave import slave_utils
from slave import svn
from telemetry import browser_backend
from telemetry import multi_page_benchmark_runner
from telemetry import wpr_modes
from telemetry import user_agent
from telemetry.browser_options import BrowserOptions
from utils import file_utils
from utils import gs_utils
from utils import misc

from build_step import PLAYBACK_CANNED_ACL
from create_page_set import ALEXA_PREFIX
from playback_dirs import ROOT_PLAYBACK_DIR_NAME
from playback_dirs import SKPICTURES_DIR_NAME


# Local archive and skp directories.
LOCAL_PLAYBACK_ROOT_DIR = os.path.join(
    tempfile.gettempdir(), ROOT_PLAYBACK_DIR_NAME)
LOCAL_REPLAY_WEBPAGES_ARCHIVE_DIR = os.path.join(
    os.path.abspath(os.path.dirname(__file__)), 'page_sets', 'data')
TMP_SKP_DIR = tempfile.mkdtemp()

# Directory containing MultiPageBenchmarks.
BENCHMARK_DIR = misc.GetAbsPath(os.path.dirname(skpicture_printer.__file__))

# Number of times we retry telemetry if there is a problem.
NUM_TIMES_TO_RETRY = 5

# The max base name length of Skp files.
MAX_SKP_BASE_NAME_LEN = 31

# Add Nexus10 to the UA_TYPE_MAPPING list in telemetry.user_agent.
user_agent.UA_TYPE_MAPPING['nexus10'] = (
    'Mozilla/5.0 (Linux; Android 4.2; Nexus 10 Build/JOP40C) '
    'AppleWebKit/535.19 (KHTML, like Gecko) Chrome/18.0.1025.166 '
    'Safari/535.19')

# Dictionary of device to platform prefixes for skp files.
DEVICE_TO_PLATFORM_PREFIX = {
    'desktop': 'desk',
    'galaxynexus': 'mobi',
    'nexus10': 'tabl'
}


def Request(self, path, timeout=None):
  """Monkey patching telemetry.browser_backend.BrowserBackend.Request

  The original request method used timeout=None which sometimes makes
  the script hang indefinitely.
  A better fix should be upstreamed to telemetry.
  """
  # pylint: disable=W0212
  url = 'http://localhost:%i/json' % self._port
  if not timeout:
    timeout = 30
  if path:
    url += '/' + path
  req = urllib2.urlopen(url, timeout=timeout)
  return req.read()


class SkPicturePlayback(object):
  """Class that archives or replays webpages and creates skps."""

  def __init__(self, parse_options):
    """Constructs a SkPicturePlayback BuildStep instance."""
    self._all_page_sets_specified = parse_options.page_sets == 'all'
    self._page_sets = self._ParsePageSets(parse_options.page_sets)

    self._dest_gsbase = parse_options.dest_gsbase
    self._record = parse_options.record == 'True'
    self._debugger = parse_options.debugger
    self._do_not_upload_to_gs = parse_options.do_not_upload_to_gs == 'True'
    self._archive_location = parse_options.archive_location
    self._ignore_exceptions = parse_options.ignore_exceptions == 'True'

    self._local_skp_dir = os.path.join(
        parse_options.output_dir, ROOT_PLAYBACK_DIR_NAME, SKPICTURES_DIR_NAME)
    self._local_record_webpages_archive_dir = os.path.join(
        parse_options.output_dir, ROOT_PLAYBACK_DIR_NAME, 'webpages_archive')

    self._trunk = parse_options.trunk
    self._svn_username = parse_options.svn_username
    self._svn_password = parse_options.svn_password

    # List of skp files generated by this script.
    self._skp_files = []

  def _ParsePageSets(self, page_sets):
    if not page_sets:
      raise ValueError('Must specify atleast one page_set!')
    elif self._all_page_sets_specified:
      # Get everything from the page_sets directory.
      return [os.path.join('page_sets', page_set)
              for page_set in os.listdir('page_sets')
              if not os.path.isdir(os.path.join('page_sets', page_set))]
    elif '*' in page_sets:
      # Explode and return the glob.
      return glob.glob(page_sets)
    else:
      return page_sets.split(',')

  def Run(self):
    """Run the SkPicturePlayback BuildStep."""

    # Ensure the right .boto file is used by gsutil.
    if not gs_utils.DoesStorageObjectExist(self._dest_gsbase):
      raise Exception(
          'Missing .boto file or .boto does not have the right credentials.'
          'Please see https://docs.google.com/a/google.com/document/d/1ZzHP6M5q'
          'ACA9nJnLqOZr2Hl0rjYqE4yQsQWAfVjKCzs/edit '
          '(may have to request access)')

    # Delete the local root directory if it already exists.
    if os.path.exists(LOCAL_PLAYBACK_ROOT_DIR):
      shutil.rmtree(LOCAL_PLAYBACK_ROOT_DIR)

    # Create the required local storage directories.
    self._CreateLocalStorageDirs()

    # Start the timer.
    start_time = time.time()

    # Sort page_sets only if they are from the alexa list with the format:
    # alexa10_webpage_desktop.json
    # This is to ensure that page sets are processed in order and not randomly.
    if self._page_sets and (
        os.path.basename(self._page_sets[0]).startswith(ALEXA_PREFIX)):
      self._page_sets = sorted(
          self._page_sets,
          key=lambda p: int(
              os.path.basename(p).split('_')[0].lstrip(ALEXA_PREFIX)))

    # Loop through all page_sets.
    for page_set in self._page_sets:

      # Check to see if multiple webpages are specified in this page_set.
      with open(page_set, 'r') as page_set_file:
        parsed_json = json.load(page_set_file)
        multiple_pages_specified = len(parsed_json['pages']) > 1

      wpr_file_name = page_set.split(os.path.sep)[-1].split('.')[0] + '.wpr'

      if not self._record:
        # Get the webpages archive so that it can be replayed.
        self._GetWebpagesArchive(wpr_file_name)

      # Clear all command line arguments and add only the ones supported by
      # the skpicture_printer benchmark.
      self._SetupArgsForSkPrinter(page_set)

      accept_skp = False
      errors_in_webpage = False

      while not accept_skp:
        # Adding retries to workaround the bug
        # https://code.google.com/p/chromium/issues/detail?id=161244.
        num_times_retried = 0
        retry = True
        while retry:
          try:
            # Run the skpicture_printer script which:
            # Creates an archive of the specified webpages if '--record' is
            # specified.
            # Saves all webpages in the page_set as skp files.
            multi_page_benchmark_runner.Main(BENCHMARK_DIR)
          except Exception, e:
            if self._ignore_exceptions:
              traceback.print_exc()
              errors_in_webpage = True
              break
            else:
              raise e
          try:
            cPickle.load(open(os.path.join(
                LOCAL_REPLAY_WEBPAGES_ARCHIVE_DIR, wpr_file_name), 'rb'))
            retry = False
          except EOFError, e:
            traceback.print_exc()
            num_times_retried += 1
            if num_times_retried > NUM_TIMES_TO_RETRY:
              if self._ignore_exceptions:
                print 'Exceeded number of times to retry!'
                errors_in_webpage = True
                break
              else:
                raise e
            else:
              print '======================Retrying!======================'

        if self._debugger:
          skp_files = glob.glob(os.path.join(TMP_SKP_DIR, '*', 'layer_0.skp'))
          for skp_file in skp_files:
            os.system('%s %s' % (self._debugger, skp_file))
          user_input = raw_input(
              "Would you like to recapture the skp(s)? [y,n]")
          accept_skp = False if user_input == 'y' else True
        else:
          # Always accept skps if debugger is not provided to preview.
          accept_skp = True

      if errors_in_webpage:
        continue

      if self._record:
        # Move over the created archive into the local webpages archive
        # directory.
        shutil.move(
            os.path.join(LOCAL_REPLAY_WEBPAGES_ARCHIVE_DIR, wpr_file_name),
            self._local_record_webpages_archive_dir)

      # Rename generated skp files into more descriptive names.
      self._RenameSkpFiles(page_set, multiple_pages_specified)

    print '\n\n=======Capturing SKP files took %s seconds=======\n\n' % (
        time.time() - start_time)

    if not self._do_not_upload_to_gs:
      # Copy the directory structure in the root directory into Google Storage.
      gs_status = slave_utils.GSUtilCopyDir(
          src_dir=LOCAL_PLAYBACK_ROOT_DIR, gs_base=self._dest_gsbase,
          dest_dir=ROOT_PLAYBACK_DIR_NAME, gs_acl=PLAYBACK_CANNED_ACL)
      if gs_status != 0:
        raise Exception(
            'ERROR: GSUtilCopyDir error %d. "%s" -> "%s/%s"' % (
                gs_status, LOCAL_PLAYBACK_ROOT_DIR, self._dest_gsbase,
                ROOT_PLAYBACK_DIR_NAME))
    
      # Add a timestamp file to the skp directory in Google Storage so we can
      # use directory level rsync like functionality.
      gs_utils.WriteTimeStampFile(
          timestamp_file_name=gs_utils.TIMESTAMP_COMPLETED_FILENAME,
          timestamp_value=time.time(),
          gs_base=self._dest_gsbase,
          gs_relative_dir=posixpath.join(ROOT_PLAYBACK_DIR_NAME,
                                         SKPICTURES_DIR_NAME),
          gs_acl=PLAYBACK_CANNED_ACL,
          local_dir=LOCAL_PLAYBACK_ROOT_DIR)

      # Submit a whitespace change if all required arguments have been provided.
      if self._trunk and self._svn_username and self._svn_password:
        repo = svn.Svn(self._trunk, self._svn_username, self._svn_password,
                       additional_svn_flags=[
                           '--trust-server-cert', '--no-auth-cache',
                           '--non-interactive'])
        whitespace_file = open(
            os.path.join(self._trunk, 'whitespace.txt'), 'a')
        try:
          whitespace_file.write('\n')
        finally:
          whitespace_file.close()
        if self._all_page_sets_specified:
          commit_msg = 'All skp files in Google Storage have been updated'
        else:
          commit_msg = (
              'Updated the following skp files on Google Storage: %s' % (
                  self._skp_files))
        # Adding a pattern that makes the commit msg show up as an annotation
        # in the dashboard. Please see for more details:
        # https://code.google.com/p/skia/issues/detail?id=1065
        commit_msg += ' (AddDashboardAnnotation)'
        # pylint: disable=W0212
        repo._RunSvnCommand(
            ['commit', '--message', commit_msg, 'whitespace.txt'])

    return 0

  def _RenameSkpFiles(self, page_set, multiple_pages_specified):
    """Rename generated skp files into more descriptive names.

    All skp files are currently called layer_X.skp where X is an integer, they
    will be renamed into http_website_name_X.skp.

    Eg: http_news_yahoo_com/layer_0.skp -> http_news_yahoo_com_0.skp
    """
    for (dirpath, _dirnames, filenames) in os.walk(TMP_SKP_DIR):
      if not dirpath or not filenames:
        continue
      basename = os.path.basename(dirpath)
      for filename in filenames:
        filename_parts = filename.split('.')
        extension = filename_parts[1]
        integer = filename_parts[0].split('_')[1]
        if integer != '0':
          # We only care about layer 0s.
          continue
        basename = basename.rstrip('_')

        # Gets the platform prefix for the page set.
        # Eg: for 'skia_yahooanswers_desktop.json' it gets 'desktop'.
        device = (page_set.split(os.path.sep)[-1].split('_')[-1].split('.')[0])
        platform_prefix = DEVICE_TO_PLATFORM_PREFIX[device]
        if multiple_pages_specified:
          # Get the webpage name from the directory name.
          # Eg: for '/tmp/tmpAADdmW/http___facebook_com' it gets 'facebook_com'.
          webpage_name = dirpath.split(os.path.sep)[-1].split('___')[-1]
        else:
          # Gets the webpage name from the page set name.
          # Eg: for 'skia_yahooanswers_desktop.json' it gets 'yahooanswers'.
          webpage_name = page_set.split(os.path.sep)[-1].split('_')[-2]

        # Construct the basename of the skp file.
        basename = '%s_%s' % (platform_prefix, webpage_name)

        # Ensure the basename is not too long.
        if len(basename) > MAX_SKP_BASE_NAME_LEN:
          basename = basename[0:MAX_SKP_BASE_NAME_LEN]
        new_filename = '%s.%s' % (basename, extension)
        shutil.move(os.path.join(dirpath, filename),
                    os.path.join(self._local_skp_dir, new_filename))
        self._skp_files.append(new_filename)
      shutil.rmtree(dirpath)

  def AddSkPicturePrinterOptions(self, parser):
    """Temporary workaround for a chromium bug.
    
    skpicture_printer.SkPicturePrinter has AddOptions but it should instead have
    AddCommandLineOptions so it can override
    page_test.PageTest.AddCommandLineOptions.
    """
    parser.add_option('--record', action='store_const',
                      dest='wpr_mode', const=wpr_modes.WPR_RECORD,
                      help='Record to the page set archive')

  def CustomizeBrowserOptions(self, browser_options):
    """Specifying Skia specific browser options."""
    browser_options.extra_browser_args.extend(['--enable-gpu-benchmarking',
                                             '--no-sandbox',
                                             '--enable-deferred-image-decoding',
                                             '--force-compositing-mode'])

  def _SetupArgsForSkPrinter(self, page_set):
    """Setup arguments for the skpicture_printer script.

    Clears all command line arguments and adds only the ones supported by
    skpicture_printer.
    """
    # Clear all command line arguments.
    del sys.argv[:]
    # Dummy first argument.
    sys.argv.append('dummy_file_name')
    if self._record:
      # Create a new wpr file.
      sys.argv.append('--record')
    # Use the system browser.
    sys.argv.append('--browser=system')
    # Specify extra browser args needed for Skia.
    skpicture_printer.SkPicturePrinter.CustomizeBrowserOptions = (
        self.CustomizeBrowserOptions)
    # Set a limit to the timeout instead of using None, else sometimes the
    # script hangs due to broken socket connections.
    browser_backend.BrowserBackend.Request = Request
    # Output skp files to skpictures_dir.
    BrowserOptions.outdir = TMP_SKP_DIR
    skpicture_printer.SkPicturePrinter.AddCommandLineOptions = (
        self.AddSkPicturePrinterOptions)

    # Point to the skpicture_printer benchmark.
    sys.argv.append('skpicture_printer')
    # Point to the top 25 webpages page set.
    sys.argv.append(page_set)

  def _CreateLocalStorageDirs(self):
    """Creates required local storage directories for this script."""
    file_utils.CreateCleanLocalDir(self._local_record_webpages_archive_dir)
    file_utils.CreateCleanLocalDir(self._local_skp_dir)

  def _GetWebpagesArchive(self, wpr_file_name):
    """Get the webpages archive."""
    if self._archive_location:
      shutil.copyfile(self._archive_location,
                      os.path.join(LOCAL_REPLAY_WEBPAGES_ARCHIVE_DIR,
                                   wpr_file_name))
    else:
      wpr_source = posixpath.join(
          self._dest_gsbase, ROOT_PLAYBACK_DIR_NAME, 'webpages_archive',
          wpr_file_name)
      if gs_utils.DoesStorageObjectExist(wpr_source):
        slave_utils.GSUtilDownloadFile(
            src=wpr_source, dst=LOCAL_REPLAY_WEBPAGES_ARCHIVE_DIR)
      else:
        raise Exception('%s does not exist in Google Storage!' % wpr_source)


if '__main__' == __name__:
  option_parser = optparse.OptionParser()
  option_parser.add_option(
      '', '--page_sets',
      help='Specifies the page sets to use to archive. Supports globs.',
      default='all')
  option_parser.add_option(
      '', '--record',
      help='Specifies whether a new website archive should be created.',
      default='False')
  option_parser.add_option(
      '', '--dest_gsbase',
      help='gs:// bucket_name, the bucket to upload the file to.',
      default='gs://chromium-skia-gm')
  option_parser.add_option(
      '', '--debugger',
      help=('Path to a debugger. You can preview a captured skp if a debugger '
            'is specified.'),
      default=None)
  option_parser.add_option(
      '', '--do_not_upload_to_gs',
      help='Does not upload to Google Storage if this is true.',
      default='False')
  option_parser.add_option(
      '', '--archive_location',
      help='Downloads from Google Storage if archive location is unspecified.')
  option_parser.add_option(
      '', '--output_dir',
      help='Directory where SKPs and webpage archives will be outputted to.',
      default=tempfile.gettempdir())
  option_parser.add_option(
      '', '--trunk',
      help='Path to Skia trunk, used for whitespace commit.',
      default=None)
  option_parser.add_option(
      '', '--svn_username',
      help='SVN username, used for whitespace commit.',
      default=None)
  option_parser.add_option(
      '', '--svn_password',
      help='SVN password, used for whitespace commit.',
      default=None)
  option_parser.add_option(
      '', '--ignore_exceptions',
      help='Does not fail the script if this is true, it instead moves on to '
           'the next page_set.',
      default='False')
  options, unused_args = option_parser.parse_args()

  playback = SkPicturePlayback(options)
  sys.exit(playback.Run())

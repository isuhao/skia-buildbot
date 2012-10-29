#!/usr/bin/env python
# Copyright (c) 2012 The Chromium Authors. All rights reserved.
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

""" Run the Skia tests executable. """

from build_step import BuildStep
from chromeos_build_step import ChromeOSBuildStep
from utils import ssh_utils
import sys


class ChromeOSRunTests(ChromeOSBuildStep):
  def _Run(self):
    ssh_utils.RunSSH(self._ssh_username, self._ssh_host, self._ssh_port,
                     ['skia_tests'])


if '__main__' == __name__:
  sys.exit(BuildStep.RunBuildStep(ChromeOSRunTests))
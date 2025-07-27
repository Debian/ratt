====
ratt
====

-----------------------
Rebuild All The Things!
-----------------------

:Author: This manual page was written by Aquila Macedo Costa <aquilamacedo@riseup.net>.
:Copyright: MIT (Expat)
:Manual section: 1
:Manual group: ratt

SYNOPSIS
========
::

   ratt [-h] [-dry_run] [-recheck] [-skip_ftbfs]
        [-include REGEX] [-exclude REGEX]
        [-dist DIST] [-sbuild_dist DIST]
        [-log_dir DIR] [-chdist NAME] <file>.changes

DESCRIPTION
===========
**ratt** (“Rebuild All The Things!”) operates on a Debian `.changes` file of a
just-built package, identifies all reverse-build-dependencies and rebuilds them
with the `.debs` from the .changes file.

The intended use-case is, for example, to package a new snapshot of a Go
library and verify that the new version does not break any other Go
libraries/binaries.

The builds are performed using ``sbuild(1)``. See https://wiki.debian.org/sbuild for instructions on setting it up.


OPTIONS
=======
**-chdist** *string*
 Use the package index files from a `chdist` environment instead of the host
 APT setup. The name must match the one used in `chdist create`.

**-dist** *string*
 Distribution to look up reverse-build-dependencies from. Defaults to the
`Distribution:` field in the `.changes` file.

**-dry_run**
 Print sbuild command lines, but do not build anything.

**-exclude** *regex*
 Exclude packages matching the given regular expression.

**-include** *regex*
 Only build packages matching the given regular expression.

**-log_dir** *string*
 Directory to store sbuild(1) logs (default: `buildlogs`).

**-recheck**
 Rebuild previously failed packages again, even without new changes.

**-sbuild_dist** *string*
 Value passed to `sbuild --dist=` (e.g., `sid`).

**-skip_ftbfs**
 Skip packages marked as FTBFS on udd.debian.org.


Using `-chdist` for Suite Isolation
===================================

To avoid modifying your system-wide `/etc/apt/sources.list`, you can use
`chdist` to simulate isolated APT environments per distribution suite.

Basic steps:

1. Create the chdist environment::

   $ chdist create bookworm http://deb.debian.org/debian bookworm main

2. Update its APT metadata::

   $ chdist bookworm apt-get update

3. Run ratt using the chdist environment::

   $ ratt -chdist bookworm yourpackage_*.changes

This will use the package index files from `~/.chdist/bookworm` instead of your system's APT configuration.

**Note**: The value passed to `-chdist` must match the name used in `chdist create`.

EXAMPLES
========

Basic::

  $ ratt yourpackage_*.changes

With chdist::

  $ ratt -chdist sid yourpackage_*.changes

Dry run::

  $ ratt -dry_run -chdist sid yourpackage_*.changes

Skip packages known FTBFS::

  $ ratt -skip_ftbfs -chdist sid yourpackage_*.changes

Filter specific packages::

  $ ratt -include '^(hwloc|fltk1.3)$' yourpackage_*.changes

Exclude expensive packages::

  $ ratt -exclude '^(gcc-9|gcc-8|llvm-toolchain)$' yourpackage_*.changes

SEE ALSO
========

**sbuild(1)**, **chdist(1)**

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
        [-dist DIST] [-sbuild_dist DIST] [-sbuild-experimental-aspcud] [-sbuild-keep-build-log]
        [-log_dir DIR] [-chdist NAME]
        [-direct-rdeps] [-rdeps-depth N]
        [-transition_affected REGEX]
        [-json] <file>.changes

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

**-sbuild-experimental-aspcud**
 Enable buildd-like experimental mode in sbuild. Add the experimental
 repository overlay and use aspcud with buildd criteria. By default, ratt
 rebuilds reverse build-dependencies from unstable and only injects the
 provided ``.deb``'s via ``sbuild --extra-package``.

**-skip_ftbfs**
 Skip packages marked as FTBFS on udd.debian.org.

**-sbuild-keep-build-log**
 Let sbuild produce its ``.build`` log. Without this option, ratt passes
 sbuild's ``--nolog`` and saves console output in ``-log_dir`` instead.

**-direct-rdeps**
 Limit the reverse dependency analysis to packages that directly Build-Depend
 on the target. Equivalent to using ``-rdeps-depth=2``.

**-rdeps-depth** *N*
 Set the maximum depth for reverse dependency resolution (via
 ``dose-ceve(1)``).  If unset, all the transitive reverse dependencies will be
 included.  See the ``--depth`` option in ``dose-ceve(1)`` manpage to see
 more details.

**-transition_affected** *regex*
 Select source packages for a transition by scanning parsed binary package
 ``Depends`` from the selected ``Packages`` indexes. If a parsed dependency
 package name matches the regex, the binary package is mapped back to its
 source package, and ratt rebuilds that source package while still injecting
 the ``.deb`` files from the required ``.changes`` file.

 This mode does not use the binaries from the ``.changes`` file as reverse
 build-dependency roots. The argument is only a regex matching parsed
 dependency package names, not a regex matched against the full raw ``Depends``
 field. Users should usually pass anchored package name regexes such as
 ``^(libfoo2|libfoo1)$``.

 If a matching binary maps to a source package that is not present in the
 selected ``Sources`` indexes, ratt logs a warning and cannot schedule that
 source for rebuild. When comparing with Ben transition pages, ensure the local
 archive metadata includes the same relevant components, such as ``contrib``
 for transition consumers that live there.

**-json**
 Output results in JSON format (currently only works in combination with
 `-dry_run`). JSON is written to stdout; human-readable logs go to stderr. Each
 entry includes the reverse build-dependency name, its version, and the
 corresponding `sbuild` command that would be executed.

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

Keep sbuild .build logs::

  $ ratt -sbuild-keep-build-log yourpackage_*.changes

Limit to direct reverse build-dependencies only::

  $ ratt -direct-rdeps yourpackage_*.changes

Transition-aware candidate selection::

  $ ratt -transition_affected '^(libfoo2|libfoo1)$' yourpackage_*.changes

Choosing the transition regex:

If a Ben transition tracker exists, start from its ``Affected`` expression. For
example, convert ``Affected: .depends ~ /\b(libfoo2|libfoo1)\b/`` to
``-transition_affected '^(libfoo2|libfoo1)$'``.

If there is no tracker yet, build the regex from the old and new runtime
library package names for the transition. Include both, since some packages may
still depend on the old package while others already depend on the new one. Do
not copy every binary from the ``.changes`` file into the regex. Leave out
unrelated packages such as ``-dev``, ``-doc``, dbgsym, or utilities, unless
they are actually part of the transition.

Print dry-run result in JSON format::

  $ ratt -dry_run -json yourpackage_*.changes

Suppress all logs and print only JSON (clean output)::

  $ ratt -dry_run -json yourpackage_*.changes 2>/dev/null

Extract only the sbuild commands (with jq)::

  $ ratt -dry_run -json yourpackage_*.changes 2>/dev/null | jq -r '.dry_run_builds[].sbuild_command'

Filter specific packages::

  $ ratt -include '^(hwloc|fltk1.3)$' yourpackage_*.changes

Exclude expensive packages::

  $ ratt -exclude '^(gcc-9|gcc-8|llvm-toolchain)$' yourpackage_*.changes

SEE ALSO
========

**sbuild(1)**, **chdist(1)**

# Description

ratt (“Rebuild All The Things!”) operates on a Debian .changes file of a just-built package, identifies all reverse-build-dependencies and rebuilds them with the .debs from the .changes file.

The intended use-case is, for example, to package a new snapshot of a Go library and verify that the new version does not break any other Go libraries/binaries.

# Installation (from git, for hacking on ratt)

Please install ratt from Debian. In case you want to hack on ratt, you can use the following commands to install Go, download ratt from git and compile/install it:

```bash
sudo apt-get install golang-go
git clone https://github.com/Debian/ratt
cd ratt
go install
```

Start the resulting binary in `~/go/bin/ratt`:

```bash
~/go/bin/ratt -help
```

After making changes to the code, to recompile and install it again, use:

```bash
go install
```

# Usage

Let’s assume you build a new version of a Go library, like so:

```bash
debcheckout golang-github-jacobsa-gcloud-dev
cd golang-github-jacobsa-gcloud-dev
dch -i -m 'dummy new version'
git commit -a -m 'dummy new version'
gbp buildpackage --git-pbuilder  
```

Now you can use ratt to identify and rebuild all reverse-build-dependencies:
```
$ ratt golang-github-jacobsa-gcloud_0.0\~git20150709-2_amd64.changes         
2015/08/16 11:48:41 Loading changes file "golang-github-jacobsa-gcloud_0.0~git20150709-2_amd64.changes"
2015/08/16 11:48:41  - 1 binary packages: golang-github-jacobsa-gcloud-dev
2015/08/16 11:48:41  - corresponding .debs (will be injected when building):
2015/08/16 11:48:41     golang-github-jacobsa-gcloud-dev_0.0~git20150709-2_all.deb
2015/08/16 11:48:41 Loading sources index "/var/lib/apt/lists/ftp.ch.debian.org_debian_dists_sid_contrib_source_Sources"
2015/08/16 11:48:41 Loading sources index "/var/lib/apt/lists/ftp.ch.debian.org_debian_dists_sid_main_source_Sources"
2015/08/16 11:48:43 Loading sources index "/var/lib/apt/lists/ftp.ch.debian.org_debian_dists_sid_non-free_source_Sources"
2015/08/16 11:48:43 Building golang-github-jacobsa-ratelimit_0.0~git20150723.0.2ca5e0c-1 (commandline: [sbuild --arch-all --dist=sid --nolog golang-github-jacobsa-ratelimit_0.0~git20150723.0.2ca5e0c-1 --extra-package=golang-github-jacobsa-gcloud-dev_0.0~git20150709-2_all.deb])
2015/08/16 11:49:19 Build results:
2015/08/16 11:49:19 PASSED: golang-github-jacobsa-ratelimit_0.0~git20150723.0.2ca5e0c-1
```

ratt uses `sbuild(1)` to build packages, see https://wiki.debian.org/sbuild for instructions on how to set up sbuild. Be sure to add `--components=main,contrib,non-free` to the sbuild-createchroot line in case you want to deal with packages outside of main as well.

# Targeting a different suite

Imagine you’re running Debian stable on your machine, but you’re working on a package for Debian unstable (“sid”). Unless you already have configured a corresponding `sources.list` entry for sid, you will encounter an error message like this:

```
$ ratt golang-google-grpc_1.11.0-1_amd64.changes
2019/01/19 10:44:34 Loading changes file "golang-google-grpc_1.11.0-1_amd64.changes"
2019/01/19 10:44:34  - 1 binary packages: golang-google-grpc-dev
2019/01/19 10:44:34 Corresponding .debs (will be injected when building):
2019/01/19 10:44:34     golang-google-grpc-dev_1.11.0-1_all.deb
2019/01/19 10:44:34 Setting -dist=sid (from .changes file)
2019/01/19 10:44:34 Could not find InRelease file for sid . Are you missing sid in your /etc/apt/sources.list?
```

The most direct solution is to add sid to your `/etc/apt/sources.list` file, then set `Default-Release` to stable, so that apt prefers the same packages as before your addition:

```
# echo 'deb http://deb.debian.org/debian sid main' >> /etc/apt/sources.list
# echo 'deb-src http://deb.debian.org/debian sid main' >> /etc/apt/sources.list
# echo 'APT::Default-Release "stable";' >> /etc/apt/apt.conf
# apt update
$ ratt ...
```

An alternative solution is to use `chdist(1)`, a tool that allows to create and maintain different apt trees for different suites. Assuming that you have a sid distribution ready in your `~/.chdist`, you can then use it by setting the environment variable `APT_CONFIG`:

```
APT_CONFIG=~/.chdist/sid/etc/apt/apt.conf ratt ...
```

# Restricting the set of packages that will be built

In some cases there are many build dependencies; to focus on a smaller set of packages, invoke ratt with the `-include` / `-exclude` options.

To only build selected packages, use:

```
ratt -recheck -include '^(hwloc|fltk1.3|wcslib|ccfits|qevercloud|libstxxl|caffe|frobby|starpu)$' ../doxygen_1.8.17-1_amd64.changes
```

To exclude certain packages (for example those with longer build times):

```
ratt -recheck -exclude '^(gcc-9|gcc-8|llvm-toolchain-10|libreoffice|trilinos|llvm-toolchain-9|llvm-toolchain-8|llvm-toolchain-7|gcc-snapshot|gcc-10|deal.ii|kodi|vg|qgis|openms|siconos|ball|gtg-trace|libsbml|dcmtk|gromacs|gudhi|kicad|libpwiz)$' ../doxygen_1.8.17-1_amd64.changes
```

**Note**: you need to escape the `+` sign in package names as in `dbus-c\+\+` to avoid messing up the regexp expression.


# Using `-chdist` to target multiple Debian suites

Instead of modifying your system-wide `/etc/apt/sources.list`, you can use
`chdist` to simulate isolated APT environments per suite.

1. Create a named distribution profile:

    ```
    chdist create <DIST> <URL> <RELEASE> <SECTIONS>
    ```

    A common example:

    ```
    chdist create bookworm http://deb.debian.org/debian bookworm main
    ```

    This creates a chdist environment named unstable.

2. Update the APT metadata

    After creating the distribution, update it:

    ```
    chdist bookworm apt-get update
    ```

    This step is mandatory, it downloads the package index files (*_Sources,
    *_Packages, etc.) that ratt will later use

3. Run ratt with the `-chdist` option

    Once the chdist environment is set up and updated, you can run ratt with the
    `-chdist` flag pointing to the name of the distribution:

    ```
    ratt -chdist bookworm yourpackage_*.changes
    ```

    This will use the index files from the chdist environment located at
    `~/.chdist/bookworm`, instead of the host system's APT config.

    **Note**: The name passed to `-chdist` refers to the profile created via `chdist
    create`

# Description

ratt (“Rebuild All The Things!”) operates on a Debian .changes file of a just-built package, identifies all reverse-build-dependencies and rebuilds them with the .debs from the .changes file.

The intended use-case is, for example, to package a new snapshot of a Go library and verify that the new version does not break any other Go libraries/binaries.

# Installation (from git, for hacking on ratt)

Please install ratt from Debian. In case you want to hack on ratt, you can use the following commands to install Go, download ratt from git and compile/install it into your `$GOPATH`:

```bash
sudo apt-get install golang-go
export GOPATH=~/gocode
go get -u github.com/Debian/ratt
```

Start the resulting binary in `~/gocode/bin/ratt`:

```bash
~/gocode/bin/ratt -help
```

After making changes to the code, to recompile and install it again, use:

```bash
go install github.com/Debian/ratt
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

# Adding build capacity to your setup

By default, ratt installs a local ratt-builder server process, listening on `localhost:12311`.

In this example, our remote computer has the hostname `x1`.

## Using a persistent SSH port forwarding

To use a remote ratt-builder server process, create a password-less SSH key,
install the key on the remote computer and add a corresponding ssh_config(5)
stanza:

```shell
REMOTE=x1
KEY=$HOME/.ssh/$(hostname)-${REMOTE?}-ratt
ssh-keygen -N '' -C "$(hostname)-${REMOTE?}-ratt" -f "${KEY?}"
ssh-copy-id -i "${KEY?}" -f "${REMOTE?}"
cat >> $HOME/.ssh/config <<EOT

Host ${REMOTE?}-ratt
	Hostname ${REMOTE?}
	IdentityFile ${KEY?}
EOT
```

Enable `ratt-autossh@.service`, which provides the UNIX socket [`$XDG_RUNTIME_DIR/ratt/x1-ratt`](https://manpages.debian.org/stretch/systemd/systemd.exec.5#ENVIRONMENT_VARIABLES_IN_SPAWNED_PROCESSES):

```shell
systemctl --user enable --now ratt-autossh@$(systemd-escape "${REMOTE?}-ratt").service
```

That’s it! `ratt(1)` will from now on distribute builds across `localhost` and `x1`.

## Known issues

1. gRPC busy-loops when the builder is not running on a reachable remote host: https://github.com/grpc/grpc-go/issues/1535

## Defense in depth: restricting SSH access to local port forwardings

To reduce the attack surface when your SSH key gets compromised, it is good practice to restrict the key on the remote end, i.e. edit ~/.ssh/authorized_keys to look like this:

```
command="/bin/sh -c 'sleep 9999d'",restrict,port-forwarding ssh-rsa AAAA… midna-x1-ratt
```

## Debugging

### No autossh enabled (local-only)

1. `ratt(1)` unsuccessfully tries to connect to `localhost:12500`, but `ratt-balancer-roundrobin.service` was not activated because `ratt-balancer-roundrobin.path` did not find any UNIX sockets in `$XDG_RUNTIME_DIR/ratt`
2. `ratt(1)` connects to `localhost:12311`, which is always provided by `ratt-builder.service`.
3. `ratt(1)` sends a `Semaphore.Acquire` request and connects to `localhost:12311` to run the build.

### autossh enabled (local and remote)

1. `ratt(1)` connects to localhost:12500, which is provided by `ratt-balancer-roundrobin.service`, which was activated by `ratt-balancer-roundrobin.path` because UNIX sockets appeared in `$XDG_RUNTIME_DIR/ratt`.
2. `ratt(1)` sends a `Semaphore.Acquire` request. `ratt-balancer-roundrobin.service` forwards the request to the next resolved backend (i.e. configured port or discovered UNIX socket).
3. `ratt(1)` connects to the backend which replied to run the build.

## Using a custom transport

If you want to use a different method of securing your transport layer, create
your own setup which makes TCP ports available on localhost.

Then, override -builder_ports on ratt-balancer-roundrobin.service:
```
% systemctl --user edit ratt-balancer-roundrobin
[Service]
ExecStart=
ExecStart=/usr/bin/ratt-balancer-roundrobin -builder_ports=12311,12312
```

## Using a custom load-balancing strategy

If round-robin balancing is not sufficient, implement your own load-balancing
strategy by forwarding the Semaphore.Acquire request as per your requirements.

An example use-case could be to spin up a cluster of virtual machines on a cloud
provider on demand.

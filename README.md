# webdavfs

## A FUSE filesystem for WEBDAV shares.

Most filesystem drivers for WebDAV shares mirror files locally and operate on
on-disk caches. Partial updates often involve downloading the full file,
modifying it, then uploading it again.

webdavfs behaves like a network filesystem and supports efficient partial I/O
when the server allows it. It also has a robust RAM-backed mode for servers
without partial write support.

For that to work, you need partial write support- and unfortunately,
there is no standard for that. See
https://blog.sphere.chronosempire.org.uk/2012/11/21/webdav-and-the-http-patch-nightmare

However, there is support in Apache (the webserver, using mod_dav) and
[SabreDav](SABREDAV-partialupdate.md) (a php webserver server library,
used by e.g. NextCloud) for partial writes. So we detect if it's Apache or
SabreDav we're talking to and then use their specific methods to partially
update files.

If no support for partial writes is detected, webdavfs still allows full
read–write operation by buffering file content in RAM per open handle, and
flushing the entire file with a single PUT on close (or fsync). There is also
an inactivity auto‑flush (default 10s) to push changes even if the application
keeps the file open.

## What is working

Basic filesystem operations.

- files: create/delete/read/write/truncate/seek
- directories: mkdir rmdir readdir
- query filesystem size (df / vfsstat)

Small files (by default ≤64 MiB) use a per‑open in‑RAM cache for reads/writes,
and upload on close/fsync/auto‑flush. Large files use ranged I/O when the
server supports partial updates.

## What is not yet working

- locking

## What will not ever work

- change permissions (all files are 644, all dirs are 755)
- change user/group
- devices / fifos / chardev / blockdev etc
- truncate(2) / ftruncate(2) shrinking a file while using ranged I/O.
  Shrinking is supported for handles using the RAM cache (small files or
  when the server lacks partial write support).

This is basically because these are mostly just missing properties
from webdav.

## What platforms does it run on

- Linux
- FreeBSD (untested, but should work)
- It might work on macos if you use [osxfuse 3](https://github.com/osxfuse/osxfuse/releases/tag/osxfuse-3.11.2). Then again it might not. This is completely unsupported. See also [this issue](https://github.com/miquels/webdavfs/issues/11).

## How to install and use.

First you need to install golang, git, fuse, and set up your environment.
For Debian:

```
$ sudo -s
Password:
# apt-get install golang git fuse
# exit
```

Now with go and git installed, get a copy of this github repository:

```
$ git clone https://github.com/miquels/webdavfs
$ cd webdavfs
```

You're now ready to build the binary:

```
$ go get
$ go build
```

And install it:

```
$ sudo -s
Password:
# cp webdavfs /sbin/mount.webdavfs
```

Using it is simple as:
```
# mount -t webdavfs -ousername=you,password=pass https://webdav.where.ever/subdir /mnt
```
On exit (Ctrl+C or SIGTERM), webdavfs unmounts the mountpoint automatically.

## Command line options

| Option | Description |
| --- | --- |
| -f | don't actually mount |
| -D | daemonize | default when called as mount.* |
| -T opts | trace options: fuse,webdav,httpreq,httphdr |
| -F file | trace file. file will be reopened when renamed, tracing will stop when file is removed |
| -o opts | mount options |

## Mount options

| Option | Description |
| --- | --- |
| allow_root		| If mounted as normal user, allow access by root |
| allow_other		| Allow access by others than the mount owner. This |
|			| also sets "default_permisions" |
| default_permissions	| As per fuse documentation |
| no_default_permissions | Don't set "default_permissions" with "allow_other" |
| ro			| Read only |
| rwdirops		| Read-write for directory operations, but no file-writing (no PUT) |
| rw			| Read-write (default) |
| uid			| User ID for filesystem |
| gid			| Group ID for filesystem. |
| mode			| Mode for files/directories on the filesystem (600, 666, etc). |
|			| Files will never have the executable bit on, directories always. |
| cookie		| Authorization Cookie (Useful for O365 Sharepoint/OneDrive for Business) |
| password		| Password of webdav user |
| username		| Username of webdav user |
| async_read		| As per fuse documentation |
| nonempty		| As per fuse documentation |
| maxconns              | Maximum number of parallel connections to the webdav
|                       | server (default 8)
| maxidleconns          | Maximum number of idle connections (default 8)
| sabredav_partialupdate | Use the sabredav partialupdate protocol even when
|                        | the remote server doesn't advertise support (DANGEROUS)
| cache_threshold        | Size in bytes above which files use non‑cached ranged
|                        | I/O (if available). Default 67108864 (64 MiB).
| cache_threshold_mb     | Same as cache_threshold but specified in MiB. |

Notes:
- If the server does not support partial writes, all files use the in‑RAM
  per‑open cache regardless of size.
- The in‑RAM cache auto‑flushes after 10 seconds of inactivity per open handle,
  or on fsync/close.

If the webdavfs program is called via `mount -t webdavfs` or as `mount.webdav`,
it will fork, re-exec and run in the background. In that case it will remove
the username and password options from the command line, and communicate them
via the environment instead.

The environment options for username and password are WEBDAV_USERNAME and
WEBDAV_PASSWORD, respectively.

In the future it will also be possible to read the credentials from a
configuration file.

## TODO

- maxconns doesn't work yet. this is complicated with the Go HTTP client.
- add configuration file
- timeout handling and interrupt handling
- we use busy-loop locking, yuck. use semaphores built on channels.
- rewrite fuse.go code to use the bazil/fuse abstraction instead of bazil/fuse/fs.  
  perhaps switch to  
  - https://github.com/hanwen/go-fuse
  - https://github.com/jacobsa/fuse

## Unix filesystem extensions for webdav.

Not ever going to happen, but if you wanted a more unix-like
experience and better performance, here are a few ideas:

- Content-Type: for unix pipes / chardevs / etc
- contentsize property (read-write)
- inodenumber property
- unix properties like uid/gid/mode
- DELETE Depth 0 for collections (no delete if non-empty)
- return updated PROPSTAT information after operations
  like PUT / DELETE / MKCOL / MOVE

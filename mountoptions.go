package main

import (
	"errors"
	"strconv"
	"strings"
)

type MountOptions struct {
	AllowRoot             bool
	AllowOther            bool
	DefaultPermissions    bool
	NoDefaultPermissions  bool
	ReadOnly              bool
	ReadWrite             bool
	ReadWriteDirOps       bool
	Uid                   uint32
	Gid                   uint32
	Mode                  uint32
	Cookie                string
	Password              string
	Username              string
	AsyncRead             bool
	NonEmpty              bool
	MaxConns              uint32
	MaxIdleConns          uint32
	SabreDavPartialUpdate bool
	CacheThreshold        uint64 // bytes; 0 => default
}

func parseUInt32(v string, base int, name string, loc *uint32) (err error) {
	n, err := strconv.ParseUint(v, base, 32)
	if err == nil {
		*loc = uint32(n)
	}
	return
}

func parseMountOptions(n string, sloppy bool) (mo MountOptions, err error) {
	if n == "" {
		return
	}

	for _, kv := range strings.Split(n, ",") {
		a := strings.SplitN(kv, "=", 2)
		v := ""
		if len(a) > 1 {
			v = a[1]
		}
		switch a[0] {
		case "allow_root":
			mo.AllowRoot = true
		case "allow_other":
			mo.AllowOther = true
		case "default_permissions":
			mo.DefaultPermissions = true
		case "no_default_permissions":
			mo.NoDefaultPermissions = true
		case "ro":
			mo.ReadOnly = true
		case "rw":
			mo.ReadWrite = true
		case "rwdirops":
			mo.ReadWriteDirOps = true
		case "uid":
			err = parseUInt32(v, 10, "uid", &mo.Uid)
		case "gid":
			err = parseUInt32(v, 10, "gid", &mo.Gid)
		case "mode":
			err = parseUInt32(v, 8, "mode", &mo.Mode)
		case "cookie":
			mo.Cookie = v
		case "password":
			mo.Password = v
		case "username":
			mo.Username = v
		case "async_read":
			mo.AsyncRead = true
		case "nonempty":
			mo.NonEmpty = true
		case "maxconns":
			err = parseUInt32(v, 10, "maxconns", &mo.MaxConns)
		case "maxidleconns":
			err = parseUInt32(v, 10, "maxidleconns", &mo.MaxIdleConns)
		case "sabredav_partialupdate":
			mo.SabreDavPartialUpdate = true
		case "cache_threshold":
			// bytes
			var v64 uint64
			v64, err = strconv.ParseUint(v, 10, 64)
			if err == nil {
				mo.CacheThreshold = v64
			}
		case "cache_threshold_mb":
			// megabytes
			var v64 uint64
			v64u32, err2 := strconv.ParseUint(v, 10, 32)
			if err2 == nil {
				v64 = uint64(v64u32) * 1024 * 1024
				mo.CacheThreshold = v64
			} else {
				err = err2
			}
		default:
			if !sloppy {
				err = errors.New(a[0] + ": unknown option")
			}
		}
		if err != nil {
			return
		}
	}
	return
}

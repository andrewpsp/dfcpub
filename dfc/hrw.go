// Package dfc is a scalable object-storage based caching system with Amazon and Google Cloud backends.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 *
 */
package dfc

import (
	"github.com/OneOfOne/xxhash"
)

const mLCG32 = 1103515245

// A variant of consistent hash based on rendezvous algorithm by Thaler and Ravishankar,
// aka highest random weight (HRW)
func uniquename(bucket, objname string) string {
	return bucket + "/" + objname
}

func HrwTarget(bucket, objname string, smap *Smap) (si *daemonInfo, errstr string) {
	if smap.countTargets() == 0 {
		errstr = "DFC cluster map is empty: no targets"
		return
	}
	name := uniquename(bucket, objname)
	var max uint64
	for id, sinfo := range smap.Tmap {
		cs := xxhash.ChecksumString64S(id+":"+name, mLCG32)
		if cs > max {
			max = cs
			si = sinfo
		}
	}
	return
}

func HrwProxy(smap *Smap, idToSkip string) (pi *daemonInfo, errstr string) {
	if smap.countProxies() == 0 {
		errstr = "DFC cluster map is empty: no proxies"
		return
	}
	var max uint64
	for id, sinfo := range smap.Pmap {
		if id == idToSkip {
			continue
		}
		cs := xxhash.ChecksumString64S(id, mLCG32)
		if cs > max {
			max = cs
			pi = sinfo
		}
	}
	return
}

func hrwMpath(bucket, objname string) (mpath string) {
	var max uint64
	name := uniquename(bucket, objname)
	for path := range ctx.mountpaths.Available {
		cs := xxhash.ChecksumString64S(path+":"+name, mLCG32)
		if cs > max {
			max = cs
			mpath = path
		}
	}
	return
}

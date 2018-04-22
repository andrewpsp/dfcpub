package main

import (
	"archive/tar"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/NVIDIA/dfcpub/dfc"
	"github.com/OneOfOne/xxhash"
)

// cli
var (
	tardir string
)

type filemd struct {
	Fullname      string `json:"fullname"`
	Xxhashcontent string `json:"xxhashcontent"`
	Idx           int    `json:"idx"`
}

type objmd map[string]*filemd // by ext
type objmds map[string]objmd  // by base
type tarmds map[string]objmds // by tarname

type walkctx struct {
	tarmds tarmds
}

func main() {
	flag.StringVar(&tardir, "tardir", "/tmp", "directory that contains tars")
	flag.Parse()

	ctx := &walkctx{tarmds: tarmds{}}

	filepath.Walk(tardir, ctx.walkf)

	b, err := json.MarshalIndent(ctx.tarmds, "", "\t")
	if err == nil {
		fmt.Println(string(b))
	}
}

func (ctx *walkctx) walkf(tarname string, osfi os.FileInfo, err error) error {
	if err != nil {
		return fmt.Errorf("walkf callback invoked with err: %v", err)
	}
	if osfi.IsDir() {
		return nil
	}
	if filepath.Ext(osfi.Name()) != ".tar" {
		return nil
	}
	file, err := os.Open(tarname)
	if err != nil {
		fmt.Printf("open err = %+v\n", err)
		return err
	}
	defer file.Close()

	ctx.tarmds[tarname] = objmds{}
	fmt.Println("DEBUG: start reading", tarname)
	err = ctx.readTar(file, ctx.tarmds[tarname])
	if err != nil {
		fmt.Printf("readTar err = %+v\n", err)
	}
	return err
}

func (ctx *walkctx) readTar(file *os.File, objmds objmds) error {
	tarReader := tar.NewReader(file)
	for idx := 0; ; {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			err := ctx.extractOne(header, tarReader, objmds, idx)
			if err != nil {
				return err
			}
			idx++
		}
	}
	return nil
}

func (ctx *walkctx) extractOne(hdr *tar.Header, r io.Reader, objmds objmds, idx int) error {
	base, ext := filepath.Base(hdr.Name), filepath.Ext(hdr.Name)
	if ext != "" {
		base = base[0 : len(base)-len(ext)]
		ext = ext[1:]
	}
	fmd := &filemd{Fullname: hdr.Name, Idx: idx}
	obj, ok := objmds[base]
	if !ok {
		obj = objmd{}
	}

	xx := xxhash.New64()
	buf, slab := dfc.AllocFromSlab(hdr.Size)
	fmd.Xxhashcontent, _ = dfc.ComputeXXHash(r, buf, xx)
	dfc.FreeToSlab(buf, slab)

	obj[ext] = fmd
	objmds[base] = obj
	return nil
}

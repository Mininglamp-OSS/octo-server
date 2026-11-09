package permissionbundle

import (
	"context"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
)

// ReadCatalogDirectory snapshots an operator-owned private staging directory.
// It is never called with a request/model path. OpenRoot pins the directory;
// explicit metadata checks additionally reject links, devices and replacement.
// Embedded maps can be used where the platform cannot attest single-link files.
func ReadCatalogDirectory(ctx context.Context, pin CatalogPin, path string) (PackageInput, error) {
	initial, e := os.Lstat(path)
	if e != nil || !initial.IsDir() || initial.Mode()&os.ModeSymlink != 0 {
		return PackageInput{}, packageError("owned directory")
	}
	root, e := os.OpenRoot(path)
	if e != nil {
		return PackageInput{}, e
	}
	defer root.Close()
	info, e := root.Stat(".")
	if e != nil || !os.SameFile(initial, info) {
		return PackageInput{}, packageError("directory replacement")
	}
	inventory := func() (map[string]os.FileInfo, error) {
		files := map[string]os.FileInfo{}
		dirs := map[string]bool{}
		var walk func(string) error
		walk = func(path string) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			f, e := root.Open(path)
			if e != nil {
				return e
			}
			entries, e := f.ReadDir(65)
			f.Close()
			if e != nil && e != io.EOF {
				return e
			}
			if len(entries) > 64 {
				return packageError("directory inventory bound")
			}
			for _, entry := range entries {
				name := entry.Name()
				if path != "." {
					name = path + "/" + name
				}
				stat, e := root.Lstat(name)
				if e != nil {
					return e
				}
				if stat.IsDir() {
					allowed := name == "modules" || name == "shared" || name == "blobs"
					for _, m := range packageModules {
						allowed = allowed || name == "modules/"+m
					}
					if !allowed {
						return packageError("unexpected directory")
					}
					dirs[name] = true
					if e := walk(name); e != nil {
						return e
					}
				} else {
					_, _, _, valid := packagePath(name)
					if !stat.Mode().IsRegular() || !singleLink(stat) || name != "manifest.json" && !valid || len(files) >= 64 {
						return packageError("file type/link/path")
					}
					files[name] = stat
				}
			}
			return nil
		}
		if e := walk("."); e != nil {
			return nil, e
		}
		expectedDirs := map[string]bool{}
		for path := range files {
			parts := strings.Split(path, "/")
			for i := 1; i < len(parts); i++ {
				expectedDirs[strings.Join(parts[:i], "/")] = true
			}
		}
		if len(expectedDirs) != len(dirs) {
			return nil, packageError("empty/extra directory")
		}
		for d := range dirs {
			if !expectedDirs[d] {
				return nil, packageError("extra directory")
			}
		}
		return files, nil
	}
	before, e := inventory()
	if e != nil {
		return PackageInput{}, e
	}
	names := make([]string, 0, len(before))
	for name := range before {
		names = append(names, name)
	}
	sort.Strings(names)
	files := map[string][]byte{}
	total := 0
	for _, name := range names {
		if ctx.Err() != nil {
			return PackageInput{}, ctx.Err()
		}
		limit := 262144
		kind, _, _, _ := packagePath(name)
		if name == "manifest.json" {
			limit = 65536
		} else if kind == "blob" {
			limit = 1048576
		}
		stat := before[name]
		if stat.Size() <= 0 || stat.Size() > int64(limit) || addWork(&total, int(stat.Size()), 8388608) != nil {
			return PackageInput{}, packageError("directory file bounds")
		}
		f, e := root.Open(name)
		if e != nil {
			return PackageInput{}, e
		}
		opened, e := f.Stat()
		if e != nil || !stableFile(stat, opened) {
			f.Close()
			return PackageInput{}, packageError("opened replacement")
		}
		b, e := io.ReadAll(io.LimitReader(f, int64(limit)+1))
		after, e2 := f.Stat()
		closeErr := f.Close()
		if e != nil || e2 != nil || closeErr != nil || len(b) != int(stat.Size()) || !stableFile(stat, after) {
			return PackageInput{}, packageError("mutable file")
		}
		files[name] = b
	}
	after, e := inventory()
	if e != nil || len(before) != len(after) {
		return PackageInput{}, packageError("changed inventory")
	}
	for name, stat := range before {
		if !stableFile(stat, after[name]) {
			return PackageInput{}, packageError("changed inventory file")
		}
	}
	if ctx.Err() != nil {
		return PackageInput{}, ctx.Err()
	}
	return PackageInput{pin, files}, nil
}
func stableFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && b.Mode().IsRegular() && singleLink(b) && os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime().Equal(b.ModTime())
}
func singleLink(info os.FileInfo) bool {
	if info == nil || info.Sys() == nil {
		return false
	}
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return false
	}
	n := value.FieldByName("Nlink")
	return n.IsValid() && n.CanUint() && n.Uint() == 1
}

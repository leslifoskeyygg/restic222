package restorer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/restic/restic/internal/archiver"
	"github.com/restic/restic/internal/fs"
	"github.com/restic/restic/internal/repository"
	"github.com/restic/restic/internal/restic"
	rtest "github.com/restic/restic/internal/test"
	"golang.org/x/sync/errgroup"
)

type Node interface{}

type Snapshot struct {
	Nodes map[string]Node
}

type File struct {
	Data       string
	Links      uint64
	Inode      uint64
	Mode       os.FileMode
	ModTime    time.Time
	attributes *FileAttributes
}

type Symlink struct {
	Target  string
	ModTime time.Time
}

type Dir struct {
	Nodes      map[string]Node
	Mode       os.FileMode
	ModTime    time.Time
	attributes *FileAttributes
}

type FileAttributes struct {
	ReadOnly  bool
	Hidden    bool
	System    bool
	Archive   bool
	Encrypted bool
}

func saveFile(t testing.TB, repo restic.BlobSaver, node File) restic.ID {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	id, _, _, err := repo.SaveBlob(ctx, restic.DataBlob, []byte(node.Data), restic.ID{}, false)
	if err != nil {
		t.Fatal(err)
	}

	return id
}

func saveDir(t testing.TB, repo restic.BlobSaver, nodes map[string]Node, inode uint64, getGenericAttributes func(attr *FileAttributes, isDir bool) (genericAttributes map[restic.GenericAttributeType]json.RawMessage)) restic.ID {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tree := &restic.Tree{}
	for name, n := range nodes {
		inode++
		switch node := n.(type) {
		case File:
			fi := n.(File).Inode
			if fi == 0 {
				fi = inode
			}
			lc := n.(File).Links
			if lc == 0 {
				lc = 1
			}
			fc := []restic.ID{}
			if len(n.(File).Data) > 0 {
				fc = append(fc, saveFile(t, repo, node))
			}
			mode := node.Mode
			if mode == 0 {
				mode = 0644
			}
			err := tree.Insert(&restic.Node{
				Type:              "file",
				Mode:              mode,
				ModTime:           node.ModTime,
				Name:              name,
				UID:               uint32(os.Getuid()),
				GID:               uint32(os.Getgid()),
				Content:           fc,
				Size:              uint64(len(n.(File).Data)),
				Inode:             fi,
				Links:             lc,
				GenericAttributes: getGenericAttributes(node.attributes, false),
			})
			rtest.OK(t, err)
		case Symlink:
			symlink := n.(Symlink)
			err := tree.Insert(&restic.Node{
				Type:       "symlink",
				Mode:       os.ModeSymlink | 0o777,
				ModTime:    symlink.ModTime,
				Name:       name,
				UID:        uint32(os.Getuid()),
				GID:        uint32(os.Getgid()),
				LinkTarget: symlink.Target,
				Inode:      inode,
				Links:      1,
			})
			rtest.OK(t, err)
		case Dir:
			id := saveDir(t, repo, node.Nodes, inode, getGenericAttributes)

			mode := node.Mode
			if mode == 0 {
				mode = 0755
			}

			err := tree.Insert(&restic.Node{
				Type:              "dir",
				Mode:              mode,
				ModTime:           node.ModTime,
				Name:              name,
				UID:               uint32(os.Getuid()),
				GID:               uint32(os.Getgid()),
				Subtree:           &id,
				GenericAttributes: getGenericAttributes(node.attributes, false),
			})
			rtest.OK(t, err)
		default:
			t.Fatalf("unknown node type %T", node)
		}
	}

	id, err := restic.SaveTree(ctx, repo, tree)
	if err != nil {
		t.Fatal(err)
	}

	return id
}

func saveSnapshot(t testing.TB, repo restic.Repository, snapshot Snapshot, getGenericAttributes func(attr *FileAttributes, isDir bool) (genericAttributes map[restic.GenericAttributeType]json.RawMessage)) (*restic.Snapshot, restic.ID) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg, wgCtx := errgroup.WithContext(ctx)
	repo.StartPackUploader(wgCtx, wg)
	treeID := saveDir(t, repo, snapshot.Nodes, 1000, getGenericAttributes)
	err := repo.Flush(ctx)
	if err != nil {
		t.Fatal(err)
	}

	sn, err := restic.NewSnapshot([]string{"test"}, nil, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	sn.Tree = &treeID
	id, err := restic.SaveSnapshot(ctx, repo, sn)
	if err != nil {
		t.Fatal(err)
	}

	return sn, id
}

var noopGetGenericAttributes = func(attr *FileAttributes, isDir bool) (genericAttributes map[restic.GenericAttributeType]json.RawMessage) {
	// No-op
	return nil
}

func TestRestorer(t *testing.T) {
	var tests = []struct {
		Snapshot
		Files      map[string]string
		ErrorsMust map[string]map[string]struct{}
		ErrorsMay  map[string]map[string]struct{}
		Select     func(item string, dstpath string, node *restic.Node) (selectForRestore bool, childMayBeSelected bool)
	}{
		// valid test cases
		{
			Snapshot: Snapshot{
				Nodes: map[string]Node{
					"foo": File{Data: "content: foo\n"},
					"dirtest": Dir{
						Nodes: map[string]Node{
							"file": File{Data: "content: file\n"},
						},
					},
				},
			},
			Files: map[string]string{
				"foo":          "content: foo\n",
				"dirtest/file": "content: file\n",
			},
		},
		{
			Snapshot: Snapshot{
				Nodes: map[string]Node{
					"top": File{Data: "toplevel file"},
					"dir": Dir{
						Nodes: map[string]Node{
							"file": File{Data: "file in dir"},
							"subdir": Dir{
								Nodes: map[string]Node{
									"file": File{Data: "file in subdir"},
								},
							},
						},
					},
				},
			},
			Files: map[string]string{
				"top":             "toplevel file",
				"dir/file":        "file in dir",
				"dir/subdir/file": "file in subdir",
			},
		},
		{
			Snapshot: Snapshot{
				Nodes: map[string]Node{
					"dir": Dir{
						Mode: 0444,
					},
					"file": File{Data: "top-level file"},
				},
			},
			Files: map[string]string{
				"file": "top-level file",
			},
		},
		{
			Snapshot: Snapshot{
				Nodes: map[string]Node{
					"dir": Dir{
						Mode: 0555,
						Nodes: map[string]Node{
							"file": File{Data: "file in dir"},
						},
					},
				},
			},
			Files: map[string]string{
				"dir/file": "file in dir",
			},
		},
		{
			Snapshot: Snapshot{
				Nodes: map[string]Node{
					"topfile": File{Data: "top-level file"},
				},
			},
			Files: map[string]string{
				"topfile": "top-level file",
			},
		},
		{
			Snapshot: Snapshot{
				Nodes: map[string]Node{
					"dir": Dir{
						Nodes: map[string]Node{
							"file": File{Data: "content: file\n"},
						},
					},
				},
			},
			Files: map[string]string{
				"dir/file": "content: file\n",
			},
			Select: func(item, dstpath string, node *restic.Node) (selectedForRestore bool, childMayBeSelected bool) {
				switch item {
				case filepath.FromSlash("/dir"):
					childMayBeSelected = true
				case filepath.FromSlash("/dir/file"):
					selectedForRestore = true
					childMayBeSelected = true
				}

				return selectedForRestore, childMayBeSelected
			},
		},

		// test cases with invalid/constructed names
		{
			Snapshot: Snapshot{
				Nodes: map[string]Node{
					`..\test`:                      File{Data: "foo\n"},
					`..\..\foo\..\bar\..\xx\test2`: File{Data: "test2\n"},
				},
			},
			ErrorsMay: map[string]map[string]struct{}{
				`/`: {
					`invalid child node name ..\test`:                      struct{}{},
					`invalid child node name ..\..\foo\..\bar\..\xx\test2`: struct{}{},
				},
			},
		},
		{
			Snapshot: Snapshot{
				Nodes: map[string]Node{
					`../test`:                      File{Data: "foo\n"},
					`../../foo/../bar/../xx/test2`: File{Data: "test2\n"},
				},
			},
			ErrorsMay: map[string]map[string]struct{}{
				`/`: {
					`invalid child node name ../test`:                      struct{}{},
					`invalid child node name ../../foo/../bar/../xx/test2`: struct{}{},
				},
			},
		},
		{
			Snapshot: Snapshot{
				Nodes: map[string]Node{
					"top": File{Data: "toplevel file"},
					"x": Dir{
						Nodes: map[string]Node{
							"file1": File{Data: "file1"},
							"..": Dir{
								Nodes: map[string]Node{
									"file2": File{Data: "file2"},
									"..": Dir{
										Nodes: map[string]Node{
											"file2": File{Data: "file2"},
										},
									},
								},
							},
						},
					},
				},
			},
			Files: map[string]string{
				"top": "toplevel file",
			},
			ErrorsMust: map[string]map[string]struct{}{
				`/x`: {
					`invalid child node name ..`: struct{}{},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run("", func(t *testing.T) {
			repo := repository.TestRepository(t)
			sn, id := saveSnapshot(t, repo, test.Snapshot, noopGetGenericAttributes)
			t.Logf("snapshot saved as %v", id.Str())

			res := NewRestorer(repo, sn, Options{})

			tempdir := rtest.TempDir(t)
			// make sure we're creating a new subdir of the tempdir
			tempdir = filepath.Join(tempdir, "target")

			res.SelectFilter = func(item, dstpath string, node *restic.Node) (selectedForRestore bool, childMayBeSelected bool) {
				t.Logf("restore %v to %v", item, dstpath)
				if !fs.HasPathPrefix(tempdir, dstpath) {
					t.Errorf("would restore %v to %v, which is not within the target dir %v",
						item, dstpath, tempdir)
					return false, false
				}

				if test.Select != nil {
					return test.Select(item, dstpath, node)
				}

				return true, true
			}

			errors := make(map[string]map[string]struct{})
			res.Error = func(location string, err error) error {
				location = filepath.ToSlash(location)
				t.Logf("restore returned error for %q: %v", location, err)
				if errors[location] == nil {
					errors[location] = make(map[string]struct{})
				}
				errors[location][err.Error()] = struct{}{}
				return nil
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			err := res.RestoreTo(ctx, tempdir)
			if err != nil {
				t.Fatal(err)
			}

			if len(test.ErrorsMust)+len(test.ErrorsMay) == 0 {
				_, err = res.VerifyFiles(ctx, tempdir)
				rtest.OK(t, err)
			}

			for location, expectedErrors := range test.ErrorsMust {
				actualErrors, ok := errors[location]
				if !ok {
					t.Errorf("expected error(s) for %v, found none", location)
					continue
				}

				rtest.Equals(t, expectedErrors, actualErrors)

				delete(errors, location)
			}

			for location, expectedErrors := range test.ErrorsMay {
				actualErrors, ok := errors[location]
				if !ok {
					continue
				}

				rtest.Equals(t, expectedErrors, actualErrors)

				delete(errors, location)
			}

			for filename, err := range errors {
				t.Errorf("unexpected error for %v found: %v", filename, err)
			}

			for filename, content := range test.Files {
				data, err := os.ReadFile(filepath.Join(tempdir, filepath.FromSlash(filename)))
				if err != nil {
					t.Errorf("unable to read file %v: %v", filename, err)
					continue
				}

				if !bytes.Equal(data, []byte(content)) {
					t.Errorf("file %v has wrong content: want %q, got %q", filename, content, data)
				}
			}
		})
	}
}

func TestRestorerRelative(t *testing.T) {
	var tests = []struct {
		Snapshot
		Files map[string]string
	}{
		{
			Snapshot: Snapshot{
				Nodes: map[string]Node{
					"foo": File{Data: "content: foo\n"},
					"dirtest": Dir{
						Nodes: map[string]Node{
							"file": File{Data: "content: file\n"},
						},
					},
				},
			},
			Files: map[string]string{
				"foo":          "content: foo\n",
				"dirtest/file": "content: file\n",
			},
		},
	}

	for _, test := range tests {
		t.Run("", func(t *testing.T) {
			repo := repository.TestRepository(t)

			sn, id := saveSnapshot(t, repo, test.Snapshot, noopGetGenericAttributes)
			t.Logf("snapshot saved as %v", id.Str())

			res := NewRestorer(repo, sn, Options{})

			tempdir := rtest.TempDir(t)
			cleanup := rtest.Chdir(t, tempdir)
			defer cleanup()

			errors := make(map[string]string)
			res.Error = func(location string, err error) error {
				t.Logf("restore returned error for %q: %v", location, err)
				errors[location] = err.Error()
				return nil
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			err := res.RestoreTo(ctx, "restore")
			if err != nil {
				t.Fatal(err)
			}
			nverified, err := res.VerifyFiles(ctx, "restore")
			rtest.OK(t, err)
			rtest.Equals(t, len(test.Files), nverified)

			for filename, err := range errors {
				t.Errorf("unexpected error for %v found: %v", filename, err)
			}

			for filename, content := range test.Files {
				data, err := os.ReadFile(filepath.Join(tempdir, "restore", filepath.FromSlash(filename)))
				if err != nil {
					t.Errorf("unable to read file %v: %v", filename, err)
					continue
				}

				if !bytes.Equal(data, []byte(content)) {
					t.Errorf("file %v has wrong content: want %q, got %q", filename, content, data)
				}
			}
		})
	}
}

type TraverseTreeCheck func(testing.TB) treeVisitor

type TreeVisit struct {
	funcName string // name of the function
	location string // location passed to the function
}

func checkVisitOrder(list []TreeVisit) TraverseTreeCheck {
	var pos int

	return func(t testing.TB) treeVisitor {
		check := func(funcName string) func(*restic.Node, string, string) error {
			return func(node *restic.Node, target, location string) error {
				if pos >= len(list) {
					t.Errorf("step %v, %v(%v): expected no more than %d function calls", pos, funcName, location, len(list))
					pos++
					return nil
				}

				v := list[pos]

				if v.funcName != funcName {
					t.Errorf("step %v, location %v: want function %v, but %v was called",
						pos, location, v.funcName, funcName)
				}

				if location != filepath.FromSlash(v.location) {
					t.Errorf("step %v: want location %v, got %v", pos, list[pos].location, location)
				}

				pos++
				return nil
			}
		}

		return treeVisitor{
			enterDir:  check("enterDir"),
			visitNode: check("visitNode"),
			leaveDir:  check("leaveDir"),
		}
	}
}

func TestRestorerTraverseTree(t *testing.T) {
	var tests = []struct {
		Snapshot
		Select  func(item string, dstpath string, node *restic.Node) (selectForRestore bool, childMayBeSelected bool)
		Visitor TraverseTreeCheck
	}{
		{
			// select everything
			Snapshot: Snapshot{
				Nodes: map[string]Node{
					"dir": Dir{Nodes: map[string]Node{
						"otherfile": File{Data: "x"},
						"subdir": Dir{Nodes: map[string]Node{
							"file": File{Data: "content: file\n"},
						}},
					}},
					"foo": File{Data: "content: foo\n"},
				},
			},
			Select: func(item string, dstpath string, node *restic.Node) (selectForRestore bool, childMayBeSelected bool) {
				return true, true
			},
			Visitor: checkVisitOrder([]TreeVisit{
				{"enterDir", "/dir"},
				{"visitNode", "/dir/otherfile"},
				{"enterDir", "/dir/subdir"},
				{"visitNode", "/dir/subdir/file"},
				{"leaveDir", "/dir/subdir"},
				{"leaveDir", "/dir"},
				{"visitNode", "/foo"},
			}),
		},

		// select only the top-level file
		{
			Snapshot: Snapshot{
				Nodes: map[string]Node{
					"dir": Dir{Nodes: map[string]Node{
						"otherfile": File{Data: "x"},
						"subdir": Dir{Nodes: map[string]Node{
							"file": File{Data: "content: file\n"},
						}},
					}},
					"foo": File{Data: "content: foo\n"},
				},
			},
			Select: func(item string, dstpath string, node *restic.Node) (selectForRestore bool, childMayBeSelected bool) {
				if item == "/foo" {
					return true, false
				}
				return false, false
			},
			Visitor: checkVisitOrder([]TreeVisit{
				{"visitNode", "/foo"},
			}),
		},
		{
			Snapshot: Snapshot{
				Nodes: map[string]Node{
					"aaa": File{Data: "content: foo\n"},
					"dir": Dir{Nodes: map[string]Node{
						"otherfile": File{Data: "x"},
						"subdir": Dir{Nodes: map[string]Node{
							"file": File{Data: "content: file\n"},
						}},
					}},
				},
			},
			Select: func(item string, dstpath string, node *restic.Node) (selectForRestore bool, childMayBeSelected bool) {
				if item == "/aaa" {
					return true, false
				}
				return false, false
			},
			Visitor: checkVisitOrder([]TreeVisit{
				{"visitNode", "/aaa"},
			}),
		},

		// select dir/
		{
			Snapshot: Snapshot{
				Nodes: map[string]Node{
					"dir": Dir{Nodes: map[string]Node{
						"otherfile": File{Data: "x"},
						"subdir": Dir{Nodes: map[string]Node{
							"file": File{Data: "content: file\n"},
						}},
					}},
					"foo": File{Data: "content: foo\n"},
				},
			},
			Select: func(item string, dstpath string, node *restic.Node) (selectForRestore bool, childMayBeSelected bool) {
				if strings.HasPrefix(item, "/dir") {
					return true, true
				}
				return false, false
			},
			Visitor: checkVisitOrder([]TreeVisit{
				{"enterDir", "/dir"},
				{"visitNode", "/dir/otherfile"},
				{"enterDir", "/dir/subdir"},
				{"visitNode", "/dir/subdir/file"},
				{"leaveDir", "/dir/subdir"},
				{"leaveDir", "/dir"},
			}),
		},

		// select only dir/otherfile
		{
			Snapshot: Snapshot{
				Nodes: map[string]Node{
					"dir": Dir{Nodes: map[string]Node{
						"otherfile": File{Data: "x"},
						"subdir": Dir{Nodes: map[string]Node{
							"file": File{Data: "content: file\n"},
						}},
					}},
					"foo": File{Data: "content: foo\n"},
				},
			},
			Select: func(item string, dstpath string, node *restic.Node) (selectForRestore bool, childMayBeSelected bool) {
				switch item {
				case "/dir":
					return false, true
				case "/dir/otherfile":
					return true, false
				default:
					return false, false
				}
			},
			Visitor: checkVisitOrder([]TreeVisit{
				{"visitNode", "/dir/otherfile"},
				{"leaveDir", "/dir"},
			}),
		},
	}

	for _, test := range tests {
		t.Run("", func(t *testing.T) {
			repo := repository.TestRepository(t)
			sn, _ := saveSnapshot(t, repo, test.Snapshot, noopGetGenericAttributes)

			res := NewRestorer(repo, sn, Options{})

			res.SelectFilter = test.Select

			tempdir := rtest.TempDir(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// make sure we're creating a new subdir of the tempdir
			target := filepath.Join(tempdir, "target")

			_, err := res.traverseTree(ctx, target, string(filepath.Separator), *sn.Tree, test.Visitor(t))
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func normalizeFileMode(mode os.FileMode) os.FileMode {
	if runtime.GOOS == "windows" {
		if mode.IsDir() {
			return 0555 | os.ModeDir
		}
		return os.FileMode(0444)
	}
	return mode
}

func checkConsistentInfo(t testing.TB, file string, fi os.FileInfo, modtime time.Time, mode os.FileMode) {
	if fi.Mode() != mode {
		t.Errorf("checking %q, Mode() returned wrong value, want 0%o, got 0%o", file, mode, fi.Mode())
	}

	if !fi.ModTime().Equal(modtime) {
		t.Errorf("checking %s, ModTime() returned wrong value, want %v, got %v", file, modtime, fi.ModTime())
	}
}

// test inspired from test case https://github.com/restic/restic/issues/1212
func TestRestorerConsistentTimestampsAndPermissions(t *testing.T) {
	timeForTest := time.Date(2019, time.January, 9, 1, 46, 40, 0, time.UTC)

	repo := repository.TestRepository(t)

	sn, _ := saveSnapshot(t, repo, Snapshot{
		Nodes: map[string]Node{
			"dir": Dir{
				Mode:    normalizeFileMode(0750 | os.ModeDir),
				ModTime: timeForTest,
				Nodes: map[string]Node{
					"file1": File{
						Mode:    normalizeFileMode(os.FileMode(0700)),
						ModTime: timeForTest,
						Data:    "content: file\n",
					},
					"anotherfile": File{
						Data: "content: file\n",
					},
					"subdir": Dir{
						Mode:    normalizeFileMode(0700 | os.ModeDir),
						ModTime: timeForTest,
						Nodes: map[string]Node{
							"file2": File{
								Mode:    normalizeFileMode(os.FileMode(0666)),
								ModTime: timeForTest,
								Links:   2,
								Inode:   1,
							},
						},
					},
				},
			},
		},
	}, noopGetGenericAttributes)

	res := NewRestorer(repo, sn, Options{})

	res.SelectFilter = func(item string, dstpath string, node *restic.Node) (selectedForRestore bool, childMayBeSelected bool) {
		switch filepath.ToSlash(item) {
		case "/dir":
			childMayBeSelected = true
		case "/dir/file1":
			selectedForRestore = true
			childMayBeSelected = false
		case "/dir/subdir":
			selectedForRestore = true
			childMayBeSelected = true
		case "/dir/subdir/file2":
			selectedForRestore = true
			childMayBeSelected = false
		}
		return selectedForRestore, childMayBeSelected
	}

	tempdir := rtest.TempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := res.RestoreTo(ctx, tempdir)
	rtest.OK(t, err)

	var testPatterns = []struct {
		path    string
		modtime time.Time
		mode    os.FileMode
	}{
		{"dir", timeForTest, normalizeFileMode(0750 | os.ModeDir)},
		{filepath.Join("dir", "file1"), timeForTest, normalizeFileMode(os.FileMode(0700))},
		{filepath.Join("dir", "subdir"), timeForTest, normalizeFileMode(0700 | os.ModeDir)},
		{filepath.Join("dir", "subdir", "file2"), timeForTest, normalizeFileMode(os.FileMode(0666))},
	}

	for _, test := range testPatterns {
		f, err := os.Stat(filepath.Join(tempdir, test.path))
		rtest.OK(t, err)
		checkConsistentInfo(t, test.path, f, test.modtime, test.mode)
	}
}

// VerifyFiles must not report cancellation of its context through res.Error.
func TestVerifyCancel(t *testing.T) {
	snapshot := Snapshot{
		Nodes: map[string]Node{
			"foo": File{Data: "content: foo\n"},
		},
	}

	repo := repository.TestRepository(t)
	sn, _ := saveSnapshot(t, repo, snapshot, noopGetGenericAttributes)

	res := NewRestorer(repo, sn, Options{})

	tempdir := rtest.TempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rtest.OK(t, res.RestoreTo(ctx, tempdir))
	err := os.WriteFile(filepath.Join(tempdir, "foo"), []byte("bar"), 0644)
	rtest.OK(t, err)

	var errs []error
	res.Error = func(filename string, err error) error {
		errs = append(errs, err)
		return err
	}

	nverified, err := res.VerifyFiles(ctx, tempdir)
	rtest.Equals(t, 0, nverified)
	rtest.Assert(t, err != nil, "nil error from VerifyFiles")
	rtest.Equals(t, 1, len(errs))
	rtest.Assert(t, strings.Contains(errs[0].Error(), "Invalid file size for"), "wrong error %q", errs[0].Error())
}

func TestRestorerSparseFiles(t *testing.T) {
	repo := repository.TestRepository(t)

	var zeros [1<<20 + 13]byte

	target := &fs.Reader{
		Mode:       0600,
		Name:       "/zeros",
		ReadCloser: io.NopCloser(bytes.NewReader(zeros[:])),
	}
	sc := archiver.NewScanner(target)
	err := sc.Scan(context.TODO(), []string{"/zeros"})
	rtest.OK(t, err)

	arch := archiver.New(repo, target, archiver.Options{})
	sn, _, _, err := arch.Snapshot(context.Background(), []string{"/zeros"},
		archiver.SnapshotOptions{})
	rtest.OK(t, err)

	res := NewRestorer(repo, sn, Options{Sparse: true})

	tempdir := rtest.TempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = res.RestoreTo(ctx, tempdir)
	rtest.OK(t, err)

	filename := filepath.Join(tempdir, "zeros")
	content, err := os.ReadFile(filename)
	rtest.OK(t, err)

	rtest.Equals(t, len(zeros[:]), len(content))
	rtest.Equals(t, zeros[:], content)

	blocks := getBlockCount(t, filename)
	if blocks < 0 {
		return
	}

	// st.Blocks is the size in 512-byte blocks.
	denseBlocks := math.Ceil(float64(len(zeros)) / 512)
	sparsity := 1 - float64(blocks)/denseBlocks

	// This should report 100% sparse. We don't assert that,
	// as the behavior of sparse writes depends on the underlying
	// file system as well as the OS.
	t.Logf("wrote %d zeros as %d blocks, %.1f%% sparse",
		len(zeros), blocks, 100*sparsity)
}

func saveSnapshotsAndOverwrite(t *testing.T, baseSnapshot Snapshot, overwriteSnapshot Snapshot, options Options) string {
	repo := repository.TestRepository(t)
	tempdir := filepath.Join(rtest.TempDir(t), "target")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// base snapshot
	sn, id := saveSnapshot(t, repo, baseSnapshot, noopGetGenericAttributes)
	t.Logf("base snapshot saved as %v", id.Str())

	res := NewRestorer(repo, sn, options)
	rtest.OK(t, res.RestoreTo(ctx, tempdir))

	// overwrite snapshot
	sn, id = saveSnapshot(t, repo, overwriteSnapshot, noopGetGenericAttributes)
	t.Logf("overwrite snapshot saved as %v", id.Str())
	res = NewRestorer(repo, sn, options)
	rtest.OK(t, res.RestoreTo(ctx, tempdir))

	_, err := res.VerifyFiles(ctx, tempdir)
	rtest.OK(t, err)

	return tempdir
}

func TestRestorerSparseOverwrite(t *testing.T) {
	baseSnapshot := Snapshot{
		Nodes: map[string]Node{
			"foo": File{Data: "content: new\n"},
		},
	}
	var zero [14]byte
	sparseSnapshot := Snapshot{
		Nodes: map[string]Node{
			"foo": File{Data: string(zero[:])},
		},
	}

	saveSnapshotsAndOverwrite(t, baseSnapshot, sparseSnapshot, Options{Sparse: true, Overwrite: OverwriteAlways})
}

func TestRestorerOverwriteBehavior(t *testing.T) {
	baseTime := time.Now()
	baseSnapshot := Snapshot{
		Nodes: map[string]Node{
			"foo": File{Data: "content: foo\n", ModTime: baseTime},
			"dirtest": Dir{
				Nodes: map[string]Node{
					"file": File{Data: "content: file\n", ModTime: baseTime},
				},
				ModTime: baseTime,
			},
		},
	}
	overwriteSnapshot := Snapshot{
		Nodes: map[string]Node{
			"foo": File{Data: "content: new\n", ModTime: baseTime.Add(time.Second)},
			"dirtest": Dir{
				Nodes: map[string]Node{
					"file": File{Data: "content: file2\n", ModTime: baseTime.Add(-time.Second)},
				},
			},
		},
	}

	var tests = []struct {
		Overwrite OverwriteBehavior
		Files     map[string]string
	}{
		{
			Overwrite: OverwriteAlways,
			Files: map[string]string{
				"foo":          "content: new\n",
				"dirtest/file": "content: file2\n",
			},
		},
		{
			Overwrite: OverwriteIfChanged,
			Files: map[string]string{
				"foo":          "content: new\n",
				"dirtest/file": "content: file2\n",
			},
		},
		{
			Overwrite: OverwriteIfNewer,
			Files: map[string]string{
				"foo":          "content: new\n",
				"dirtest/file": "content: file\n",
			},
		},
		{
			Overwrite: OverwriteNever,
			Files: map[string]string{
				"foo":          "content: foo\n",
				"dirtest/file": "content: file\n",
			},
		},
	}

	for _, test := range tests {
		t.Run("", func(t *testing.T) {
			tempdir := saveSnapshotsAndOverwrite(t, baseSnapshot, overwriteSnapshot, Options{Overwrite: test.Overwrite})

			for filename, content := range test.Files {
				data, err := os.ReadFile(filepath.Join(tempdir, filepath.FromSlash(filename)))
				if err != nil {
					t.Errorf("unable to read file %v: %v", filename, err)
					continue
				}

				if !bytes.Equal(data, []byte(content)) {
					t.Errorf("file %v has wrong content: want %q, got %q", filename, content, data)
				}
			}
		})
	}
}

func TestRestorerOverwriteSpecial(t *testing.T) {
	baseTime := time.Now()
	baseSnapshot := Snapshot{
		Nodes: map[string]Node{
			"dirtest":  Dir{ModTime: baseTime},
			"link":     Symlink{Target: "foo", ModTime: baseTime},
			"file":     File{Data: "content: file\n", Inode: 42, Links: 2, ModTime: baseTime},
			"hardlink": File{Data: "content: file\n", Inode: 42, Links: 2, ModTime: baseTime},
			"newdir":   File{Data: "content: dir\n", ModTime: baseTime},
		},
	}
	overwriteSnapshot := Snapshot{
		Nodes: map[string]Node{
			"dirtest":  Symlink{Target: "foo", ModTime: baseTime},
			"link":     File{Data: "content: link\n", Inode: 42, Links: 2, ModTime: baseTime.Add(time.Second)},
			"file":     Symlink{Target: "foo2", ModTime: baseTime},
			"hardlink": File{Data: "content: link\n", Inode: 42, Links: 2, ModTime: baseTime.Add(time.Second)},
			"newdir":   Dir{ModTime: baseTime},
		},
	}

	files := map[string]string{
		"link":     "content: link\n",
		"hardlink": "content: link\n",
	}
	links := map[string]string{
		"dirtest": "foo",
		"file":    "foo2",
	}

	tempdir := saveSnapshotsAndOverwrite(t, baseSnapshot, overwriteSnapshot, Options{Overwrite: OverwriteAlways})

	for filename, content := range files {
		data, err := os.ReadFile(filepath.Join(tempdir, filepath.FromSlash(filename)))
		if err != nil {
			t.Errorf("unable to read file %v: %v", filename, err)
			continue
		}

		if !bytes.Equal(data, []byte(content)) {
			t.Errorf("file %v has wrong content: want %q, got %q", filename, content, data)
		}
	}
	for filename, target := range links {
		link, err := fs.Readlink(filepath.Join(tempdir, filepath.FromSlash(filename)))
		rtest.OK(t, err)
		rtest.Equals(t, link, target, "wrong symlink target")
	}
}

func TestRestoreModified(t *testing.T) {
	// overwrite files between snapshots and also change their filesize
	snapshots := []Snapshot{
		{
			Nodes: map[string]Node{
				"foo": File{Data: "content: foo\n", ModTime: time.Now()},
				"bar": File{Data: "content: a\n", ModTime: time.Now()},
			},
		},
		{
			Nodes: map[string]Node{
				"foo": File{Data: "content: a\n", ModTime: time.Now()},
				"bar": File{Data: "content: bar\n", ModTime: time.Now()},
			},
		},
	}

	repo := repository.TestRepository(t)
	tempdir := filepath.Join(rtest.TempDir(t), "target")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, snapshot := range snapshots {
		sn, id := saveSnapshot(t, repo, snapshot, noopGetGenericAttributes)
		t.Logf("snapshot saved as %v", id.Str())

		res := NewRestorer(repo, sn, Options{Overwrite: OverwriteIfChanged})
		rtest.OK(t, res.RestoreTo(ctx, tempdir))
		n, err := res.VerifyFiles(ctx, tempdir)
		rtest.OK(t, err)
		rtest.Equals(t, 2, n, "unexpected number of verified files")
	}
}

func TestRestoreIfChanged(t *testing.T) {
	origData := "content: foo\n"
	modData := "content: bar\n"
	rtest.Equals(t, len(modData), len(origData), "broken testcase")
	snapshot := Snapshot{
		Nodes: map[string]Node{
			"foo": File{Data: origData, ModTime: time.Now()},
		},
	}

	repo := repository.TestRepository(t)
	tempdir := filepath.Join(rtest.TempDir(t), "target")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sn, id := saveSnapshot(t, repo, snapshot, noopGetGenericAttributes)
	t.Logf("snapshot saved as %v", id.Str())

	res := NewRestorer(repo, sn, Options{})
	rtest.OK(t, res.RestoreTo(ctx, tempdir))

	// modify file but maintain size and timestamp
	path := filepath.Join(tempdir, "foo")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	rtest.OK(t, err)
	fi, err := f.Stat()
	rtest.OK(t, err)
	_, err = f.Write([]byte(modData))
	rtest.OK(t, err)
	rtest.OK(t, f.Close())
	var utimes = [...]syscall.Timespec{
		syscall.NsecToTimespec(fi.ModTime().UnixNano()),
		syscall.NsecToTimespec(fi.ModTime().UnixNano()),
	}
	rtest.OK(t, syscall.UtimesNano(path, utimes[:]))

	for _, overwrite := range []OverwriteBehavior{OverwriteIfChanged, OverwriteAlways} {
		res = NewRestorer(repo, sn, Options{Overwrite: overwrite})
		rtest.OK(t, res.RestoreTo(ctx, tempdir))
		data, err := os.ReadFile(path)
		rtest.OK(t, err)
		if overwrite == OverwriteAlways {
			// restore should notice the changed file content
			rtest.Equals(t, origData, string(data), "expected original file content")
		} else {
			// restore should not have noticed the changed file content
			rtest.Equals(t, modData, string(data), "expected modified file content")
		}
	}
}

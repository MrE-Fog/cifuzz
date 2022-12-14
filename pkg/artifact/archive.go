package artifact

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/pkg/errors"
	"golang.org/x/exp/maps"
)

// WriteArchive writes a GZip-compressed TAR to out containing the files and directories given in manifest.
// The keys in manifest correspond to the path within the archive, the corresponding value is expected to be the
// absolute path of the file or directory on disk.
// Note: WriteArchive *does not* (recursively) traverse directories to add their contents to the archive. If this is
// desired, use AddDirToManifest to explicitly add the contents to the manifest before calling WriteArchive.
func WriteArchive(out io.Writer, manifest map[string]string) error {
	gw := gzip.NewWriter(out)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	// Sort the archive paths first so that the generated archive is deterministic - map traversals aren't.
	archivePaths := maps.Keys(manifest)
	sort.Strings(archivePaths)
	for _, archivePath := range archivePaths {
		absPath := manifest[archivePath]
		err := addToArchive(tw, archivePath, absPath)
		if err != nil {
			return err
		}
	}

	return nil
}

// AddDirToManifest traverses the directory dir recursively and adds its contents to the manifest under the base path
// archiveBasePath.
func AddDirToManifest(manifest map[string]string, archiveBasePath string, dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return errors.WithStack(err)
		}
		archivePath := filepath.Join(archiveBasePath, relPath)
		// There is no harm in creating tar entries for non-empty directories, even though they are not necessary.
		manifest[archivePath] = path
		return nil
	})
}

// ExtractArchiveForTestsOnly extracts the GZip-compressed TAR read by in into dir.
func ExtractArchiveForTestsOnly(in io.Reader, dir string) error {
	gr, err := gzip.NewReader(in)
	if err != nil {
		return errors.WithStack(err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)

	for {
		var header *tar.Header
		header, err = tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.WithStack(err)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(filepath.Join(dir, header.Name), 0755)
			if err != nil {
				return errors.WithStack(err)
			}
		case tar.TypeReg:
			err = func() error {
				filePath := filepath.Join(dir, header.Name)
				err = os.MkdirAll(filepath.Dir(filePath), 0755)
				if err != nil {
					return errors.WithStack(err)
				}
				var file *os.File
				file, err = os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY, os.FileMode(header.Mode))
				if err != nil {
					return errors.WithStack(err)
				}
				defer file.Close()
				_, err = io.Copy(file, tr)
				if err != nil {
					return errors.WithStack(err)
				}
				return nil
			}()
			if err != nil {
				return err
			}
		default:
			return errors.Errorf("unsupported file type: %d", header.Typeflag)
		}
	}
	return nil
}

// addToArchive adds the file absPath to the archive under the path archivePath.
func addToArchive(tw *tar.Writer, archivePath, absPath string) error {
	fileOrDir, err := os.Open(absPath)
	if err != nil {
		return errors.Wrapf(err, "failed to add %q at %q", absPath, archivePath)
	}
	defer fileOrDir.Close()
	info, err := fileOrDir.Stat()
	if err != nil {
		return errors.WithStack(err)
	}

	// Since fileOrDir.Stat() follows symlinks, info will not be of type symlink
	// at this point - no need to pass in a non-empty value for link.
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return errors.WithStack(err)
	}
	header.Name = archivePath
	err = tw.WriteHeader(header)
	if err != nil {
		return errors.WithStack(err)
	}

	if !info.Mode().IsRegular() {
		return nil
	}
	_, err = io.Copy(tw, fileOrDir)
	if err != nil {
		return errors.Wrapf(err, "failed to compress file: %s", absPath)
	}

	return nil
}

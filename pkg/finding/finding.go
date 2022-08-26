package finding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alexflint/go-filemutex"
	"github.com/otiai10/copy"
	"github.com/pkg/errors"

	"code-intelligence.com/cifuzz/internal/cmd/run/report_handler/stacktrace"
	"code-intelligence.com/cifuzz/pkg/log"
	"code-intelligence.com/cifuzz/util/fileutil"
	"code-intelligence.com/cifuzz/util/sliceutil"
)

const nameCrashingInput = "crashing-input"
const nameJsonFile = "finding.json"
const nameFindingsDir = ".cifuzz-findings"
const lockFile = ".lock"

type Finding struct {
	Name               string        `json:"name,omitempty"`
	Type               ErrorType     `json:"type,omitempty"`
	InputData          []byte        `json:"input_data,omitempty"`
	Logs               []string      `json:"logs,omitempty"`
	Details            string        `json:"details,omitempty"`
	HumanReadableInput string        `json:"human_readable_input,omitempty"`
	MoreDetails        *ErrorDetails `json:"more_details,omitempty"`
	Tag                uint64        `json:"tag,omitempty"`
	ShortDescription   string        `json:"short_description,omitempty"`
	InputFile          string

	// Note: The following fields don't exist in the protobuf
	// representation used in the Code Intelligence core repository.
	CreatedAt  time.Time                `json:"created_at,omitempty"`
	StackTrace []*stacktrace.StackFrame `json:"stack_trace,omitempty"`

	seedPath string
}

type ErrorType string

// These constants must have this exact value (in uppercase) to be able
// to parse JSON-marshalled reports as protobuf reports which use an
// enum for this field.
const (
	ErrorType_UNKNOWN_ERROR     ErrorType = "UNKNOWN_ERROR"
	ErrorType_COMPILATION_ERROR ErrorType = "COMPILATION_ERROR"
	ErrorType_CRASH             ErrorType = "CRASH"
	ErrorType_WARNING           ErrorType = "WARNING"
	ErrorType_RUNTIME_ERROR     ErrorType = "RUNTIME_ERROR"
)

type ErrorDetails struct {
	Id       string    `json:"id,omitempty"`
	Name     string    `json:"name,omitempty"`
	Severity *Severity `json:"severity,omitempty"`
}

type Severity struct {
	Description string  `json:"description,omitempty"`
	Score       float32 `json:"score,omitempty"`
}

func (f *Finding) GetDetails() string {
	if f != nil {
		return f.Details
	}
	return ""
}

func (f *Finding) GetSeedPath() string {
	if f != nil {
		return f.seedPath
	}
	return ""
}

// Exists returns whether the JSON file of this finding already exists
func (f *Finding) Exists(projectDir string) (bool, error) {
	jsonPath := filepath.Join(projectDir, nameFindingsDir, f.Name, nameJsonFile)
	return fileutil.Exists(jsonPath)
}

func (f *Finding) Save(projectDir string) error {
	findingDir := filepath.Join(projectDir, nameFindingsDir, f.Name)

	err := os.MkdirAll(findingDir, 0755)
	if err != nil {
		return errors.WithStack(err)
	}

	// If a finding of the same name already exists, we found a duplicate.
	// We let the caller handle that by returning an AlreadyExistsError.
	jsonPath := filepath.Join(findingDir, nameJsonFile)
	exists, err := fileutil.Exists(jsonPath)
	if err != nil {
		return err
	}
	if exists {
		return WrapAlreadyExistsError(errors.Errorf("Finding %s already exists", f.Name))
	}

	if err := f.saveJson(jsonPath); err != nil {
		return err
	}

	return nil
}

func (f *Finding) saveJson(jsonPath string) error {
	bytes, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return errors.WithStack(err)
	}

	if err := os.WriteFile(jsonPath, bytes, 0644); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// MoveInputFile copies the input file to the finding directory and
// the seed corpus directory and adjusts the finding logs accordingly.
func (f *Finding) MoveInputFile(projectDir, seedCorpusDir string) error {
	// Acquire a file lock to avoid races with other cifuzz processes
	// running in parallel
	findingDir := filepath.Join(projectDir, nameFindingsDir, f.Name)
	err := os.MkdirAll(findingDir, 0755)
	if err != nil {
		return errors.WithStack(err)
	}
	lockFile := filepath.Join(findingDir, lockFile)
	mutex, err := filemutex.New(lockFile)
	if err != nil {
		return errors.WithStack(err)
	}
	err = mutex.Lock()
	if err != nil {
		return errors.WithStack(err)
	}

	// Actually move the input file
	err = f.moveInputFile(projectDir, seedCorpusDir)

	// Release the file lock
	unlockErr := mutex.Unlock()
	if err == nil {
		return errors.WithStack(unlockErr)
	}
	if unlockErr != nil {
		log.Error(unlockErr)
	}
	return err
}

func (f *Finding) moveInputFile(projectDir, seedCorpusDir string) error {
	findingDir := filepath.Join(projectDir, nameFindingsDir, f.Name)

	// Choose the new name of the input file. If the finding already
	// exists and we just found another input which causes the same
	// crash, we copy the input file to the existing finding directory
	// and increase the number at the end of the filename.
	var path string
	i := 1
	for {
		path = filepath.Join(findingDir, nameCrashingInput+"-"+strconv.Itoa(i))
		exists, err := fileutil.Exists(path)
		if err != nil {
			return err
		}
		if !exists {
			// We found a filename which doesn't exist yet
			break
		}

		// Check if the existing input file and the new file are
		// identical
		contentExistingFile, err := os.ReadFile(path)
		if err != nil {
			return errors.WithStack(err)
		}
		contentNewFile, err := os.ReadFile(f.InputFile)
		if err != nil {
			return errors.WithStack(err)
		}
		if sliceutil.Equal(contentExistingFile, contentNewFile) {
			// The input file already exists in the finding
			// directory, so we don't copy it there again.
			// We also don't copy it to the seed corpus, because
			// either it's already there, or the user removed it
			// from there deliberately, so we shouldn't it add it
			// again.
			return nil
		}

		i += 1
	}

	// Copy the input file to the finding dir. We don't use os.Rename to
	// avoid errors when source and target are not on the same mounted
	// filesystem.
	err := copy.Copy(f.InputFile, path)
	if err != nil {
		return errors.WithStack(err)
	}

	// Copy the input file to the seed corpus dir. We reuse the number
	// from the filename in the finding dir to make it more obvious that
	// the input file in the seed corpus is the same as the input
	// file in the finding dir.
	err = os.MkdirAll(seedCorpusDir, 0755)
	if err != nil {
		return errors.WithStack(err)
	}
	f.seedPath = filepath.Join(seedCorpusDir, f.Name+"-"+strconv.Itoa(i))
	err = copy.Copy(f.InputFile, f.seedPath)
	if err != nil {
		return errors.WithStack(err)
	}

	// Remove the source which was now copied.
	err = os.Remove(f.InputFile)
	if err != nil {
		return errors.WithStack(err)
	}

	// Replace the old filename in the finding logs. Replace it with the
	// relative path to not leak the directory structure of the current
	// user in the finding logs (which might be shared with others).
	cwd, err := os.Getwd()
	if err != nil {
		return errors.WithStack(err)
	}
	relPath, err := filepath.Rel(cwd, path)
	if err != nil {
		return errors.WithStack(err)
	}
	for i, line := range f.Logs {
		f.Logs[i] = strings.ReplaceAll(line, f.InputFile, relPath)
	}
	log.Debugf("moved input file from %s to %s", f.InputFile, path)

	// The path in the InputFile field is expected to be relative to the
	// project directory
	pathRelativeToProjectDir, err := filepath.Rel(projectDir, path)
	if err != nil {
		return errors.WithStack(err)
	}
	f.InputFile = pathRelativeToProjectDir
	return nil
}

// ListFindings parses the JSON files of all findings and returns the
// result.
func ListFindings(projectDir string) ([]*Finding, error) {
	findingsDir := filepath.Join(projectDir, nameFindingsDir)
	entries, err := os.ReadDir(findingsDir)
	if os.IsNotExist(err) {
		return []*Finding{}, nil
	}
	if err != nil {
		return nil, errors.WithStack(err)
	}

	var res []*Finding
	for _, e := range entries {
		f, err := LoadFinding(projectDir, e.Name())
		if err != nil {
			return nil, err
		}
		res = append(res, f)
	}

	// Sort the findings by date, starting with the newest
	sort.SliceStable(res, func(i, j int) bool {
		return res[i].CreatedAt.After(res[j].CreatedAt)
	})

	return res, nil
}

// LoadFinding parses the JSON file of the specified finding and returns
// the result.
// If the specified finding does not exist, a NotExistError is returned.
func LoadFinding(projectDir, findingName string) (*Finding, error) {
	findingDir := filepath.Join(projectDir, nameFindingsDir, findingName)
	jsonPath := filepath.Join(findingDir, nameJsonFile)
	bytes, err := os.ReadFile(jsonPath)
	if os.IsNotExist(err) {
		return nil, WrapNotExistError(err)
	}
	if err != nil {
		return nil, errors.WithStack(err)
	}
	var f Finding
	err = json.Unmarshal(bytes, &f)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &f, nil
}
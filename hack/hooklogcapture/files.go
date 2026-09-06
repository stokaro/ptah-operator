package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const maxErrorBytes = 16 << 10

type captureStatus string

const (
	statusStarting    captureStatus = "starting"
	statusWatching    captureStatus = "watching"
	statusJobObserved captureStatus = "job-observed"
	statusPodObserved captureStatus = "pod-observed"
	statusStreaming   captureStatus = "streaming"
	statusCaptured    captureStatus = "captured"
	statusFailed      captureStatus = "failed"
	statusCanceled    captureStatus = "canceled"
)

var validStatuses = map[captureStatus]struct{}{
	statusStarting:    {},
	statusWatching:    {},
	statusJobObserved: {},
	statusPodObserved: {},
	statusStreaming:   {},
	statusCaptured:    {},
	statusFailed:      {},
	statusCanceled:    {},
}

type outputPaths struct {
	log          string
	status       string
	ready        string
	error        string
	failureClass string
}

type privatePath struct {
	path string
	mu   sync.Mutex
}

type captureOutputs struct {
	log            *os.File
	logPath        string
	quarantinePath string
	status         *privatePath
	ready          *privatePath
	error          *privatePath
	failureClass   *privatePath
	// reached is the last nonterminal capture phase. The separate failure-class
	// file identifies the bounded cause category; retaining the phase as well
	// distinguishes a failure before the watches armed from one after streaming
	// began without publishing cluster-derived error text.
	reached captureStatus
}

func prepareOutputs(paths outputPaths) (*captureOutputs, error) {
	output := &captureOutputs{}
	cleaned, err := uniqueOutputPaths(paths)
	if err != nil {
		bestEffortStartupError(paths.error, err)
		bestEffortStartupFailureClass(paths.failureClass, failureClassOutput)
		return nil, err
	}

	prepared := make([]*os.File, 0, 5)
	closePrepared := func() {
		for _, file := range prepared {
			_ = file.Close()
		}
	}
	prepare := func(path string) (*os.File, error) {
		file, err := openPrivateDestinationForValidation(path)
		if err == nil {
			prepared = append(prepared, file)
		}
		return file, err
	}

	errorFile, err := prepare(cleaned.error)
	if err != nil {
		closePrepared()
		return nil, fmt.Errorf("prepare error destination: %w", err)
	}
	output.error = &privatePath{path: cleaned.error}
	failureClassFile, err := prepare(cleaned.failureClass)
	if err != nil {
		closePrepared()
		return output, fmt.Errorf("prepare failure class destination: %w", err)
	}
	output.failureClass = &privatePath{path: cleaned.failureClass}
	statusFile, err := prepare(cleaned.status)
	if err != nil {
		closePrepared()
		return output, fmt.Errorf("prepare status destination: %w", err)
	}
	readyFile, err := prepare(cleaned.ready)
	if err != nil {
		closePrepared()
		return output, fmt.Errorf("prepare ready destination: %w", err)
	}
	logFile, err := prepare(cleaned.log)
	if err != nil {
		closePrepared()
		return output, fmt.Errorf("prepare log destination: %w", err)
	}

	for i := range prepared {
		left, statErr := prepared[i].Stat()
		if statErr != nil {
			closePrepared()
			return output, fmt.Errorf("inspect destination: %w", statErr)
		}
		for j := 0; j < i; j++ {
			right, statErr := prepared[j].Stat()
			if statErr != nil {
				closePrepared()
				return output, fmt.Errorf("inspect destination: %w", statErr)
			}
			if os.SameFile(left, right) {
				closePrepared()
				return output, errors.New("destination paths must not refer to the same file")
			}
		}
	}
	for _, file := range prepared {
		if err := truncatePrivateDestination(file); err != nil {
			closePrepared()
			return output, fmt.Errorf("truncate destination: %w", err)
		}
	}
	output.logPath = cleaned.log
	output.status = &privatePath{path: cleaned.status}
	output.ready = &privatePath{path: cleaned.ready}

	if err := statusFile.Close(); err != nil {
		closePrepared()
		return output, fmt.Errorf("close status destination: %w", err)
	}
	if err := readyFile.Close(); err != nil {
		closePrepared()
		return output, fmt.Errorf("close ready destination: %w", err)
	}
	if err := errorFile.Close(); err != nil {
		closePrepared()
		return output, fmt.Errorf("close error destination: %w", err)
	}
	if err := failureClassFile.Close(); err != nil {
		closePrepared()
		return output, fmt.Errorf("close failure class destination: %w", err)
	}
	if err := logFile.Close(); err != nil {
		closePrepared()
		return output, fmt.Errorf("close log destination: %w", err)
	}
	prepared = nil

	quarantine, err := os.CreateTemp(filepath.Dir(cleaned.log), "."+filepath.Base(cleaned.log)+".quarantine-")
	if err != nil {
		return output, fmt.Errorf("create private log quarantine: %w", err)
	}
	output.log = quarantine
	output.quarantinePath = quarantine.Name()
	if err := quarantine.Chmod(0o600); err != nil {
		_ = output.close()
		return output, fmt.Errorf("protect private log quarantine: %w", err)
	}
	if err := validateOpenPrivateFile(output.quarantinePath, nil, quarantine); err != nil {
		_ = output.close()
		return output, fmt.Errorf("validate private log quarantine: %w", err)
	}
	return output, nil
}

func uniqueOutputPaths(paths outputPaths) (outputPaths, error) {
	values := []*string{&paths.log, &paths.status, &paths.ready, &paths.error, &paths.failureClass}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		absolute, err := filepath.Abs(*value)
		if err != nil {
			return outputPaths{}, fmt.Errorf("resolve destination path: %w", err)
		}
		absolute = filepath.Clean(absolute)
		if _, found := seen[absolute]; found {
			return outputPaths{}, errors.New("destination paths must be distinct")
		}
		seen[absolute] = struct{}{}
		*value = absolute
	}
	return paths, nil
}

func openPrivateDestination(path string) (*os.File, error) {
	file, err := openPrivateDestinationForValidation(path)
	if err != nil {
		return nil, err
	}
	if err := truncatePrivateDestination(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openPrivateDestinationForValidation(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|secureOpenFlags, 0o600)
		if createErr != nil {
			return nil, createErr
		}
		if validateErr := validateOpenPrivateFile(path, nil, file); validateErr != nil {
			_ = file.Close()
			return nil, validateErr
		}
		return file, nil
	}
	if err != nil {
		return nil, err
	}
	if err := validatePrivateFileInfo(path, before); err != nil {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_WRONLY|secureOpenFlags, 0)
	if err != nil {
		return nil, err
	}
	if err := validateOpenPrivateFile(path, before, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func truncatePrivateDestination(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	return nil
}

func validateOpenPrivateFile(path string, before os.FileInfo, file *os.File) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if err := validatePrivateFileInfo(path, opened); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := validatePrivateFileInfo(path, current); err != nil {
		return err
	}
	if !os.SameFile(opened, current) || (before != nil && !os.SameFile(opened, before)) {
		return errors.New("destination changed while it was opened")
	}
	return nil
}

func validatePrivateFileInfo(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("destination %q must not be a symbolic link", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("destination %q must be a regular file", path)
	}
	if err := validatePrivatePermissions(path, info); err != nil {
		return err
	}
	return nil
}

func (path *privatePath) writeAtomic(data []byte) error {
	path.mu.Lock()
	defer path.mu.Unlock()

	current, err := os.Lstat(path.path)
	if err != nil {
		return err
	}
	if err := validatePrivateFileInfo(path.path, current); err != nil {
		return err
	}

	directory := filepath.Dir(path.path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path.path)+".tmp-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}

	current, err = os.Lstat(path.path)
	if err != nil {
		return err
	}
	if err := validatePrivateFileInfo(path.path, current); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path.path); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

func (output *captureOutputs) setStatus(status captureStatus) error {
	if _, found := validStatuses[status]; !found {
		return errors.New("invalid capture status")
	}
	if output == nil || output.status == nil {
		return errors.New("status destination is unavailable")
	}
	if err := output.status.writeAtomic([]byte(string(status) + "\n")); err != nil {
		return err
	}
	if status != statusFailed && status != statusCanceled {
		output.reached = status
	}
	return nil
}

func (output *captureOutputs) markReady() error {
	if output == nil || output.ready == nil {
		return errors.New("ready destination is unavailable")
	}
	return output.ready.writeAtomic([]byte("ready\n"))
}

func (output *captureOutputs) reportFailure(err error) {
	if output == nil {
		return
	}
	_ = output.writeFailureClass(failureClassFor(err))
	_ = output.setTerminalStatus(statusFailed)
	_ = output.writeError(err)
}

func (output *captureOutputs) reportCanceled(err error) {
	if output == nil {
		return
	}
	_ = output.writeFailureClass(failureClassCanceled)
	_ = output.setTerminalStatus(statusCanceled)
	_ = output.writeError(err)
}

// setTerminalStatus records the outcome on line one and the last safe,
// allowlisted capture phase on line two. Existing outcome readers consume only
// line one; diagnostics can use line two without exposing the private cause.
func (output *captureOutputs) setTerminalStatus(status captureStatus) error {
	if _, found := validStatuses[status]; !found {
		return errors.New("invalid capture status")
	}
	if output == nil || output.status == nil {
		return errors.New("status destination is unavailable")
	}
	reached := output.reached
	if reached == "" {
		reached = statusStarting
	}
	return output.status.writeAtomic([]byte(string(status) + "\n" + string(reached) + "\n"))
}

func (output *captureOutputs) writeFailureClass(class failureClass) error {
	if _, found := validFailureClasses[class]; !found {
		return errors.New("invalid failure class")
	}
	if output == nil || output.failureClass == nil {
		return errors.New("failure class destination is unavailable")
	}
	return output.failureClass.writeAtomic([]byte(string(class) + "\n"))
}

func (output *captureOutputs) writeError(err error) error {
	if output == nil || output.error == nil || err == nil {
		return nil
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > maxErrorBytes {
		message = message[:maxErrorBytes]
	}
	return output.error.writeAtomic([]byte(message + "\n"))
}

func (output *captureOutputs) close() error {
	if output == nil {
		return nil
	}
	var result error
	if output.log != nil {
		if err := output.log.Sync(); err != nil {
			result = errors.Join(result, err)
		}
		if err := output.log.Close(); err != nil {
			result = errors.Join(result, err)
		}
		output.log = nil
	}
	if output.quarantinePath != "" {
		if err := os.Remove(output.quarantinePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
		output.quarantinePath = ""
	}
	return result
}

func (output *captureOutputs) validateLogDestination() error {
	if output == nil || output.log == nil || output.logPath == "" || output.quarantinePath == "" {
		return errors.New("log destination is unavailable")
	}
	final, err := os.Lstat(output.logPath)
	if err != nil {
		return err
	}
	if err := validatePrivateFileInfo(output.logPath, final); err != nil {
		return err
	}
	return validateOpenPrivateFile(output.quarantinePath, nil, output.log)
}

func (output *captureOutputs) publishLog() error {
	if err := output.validateLogDestination(); err != nil {
		return err
	}
	if err := output.log.Sync(); err != nil {
		return err
	}
	if err := output.log.Close(); err != nil {
		return err
	}
	output.log = nil
	final, err := os.Lstat(output.logPath)
	if err != nil {
		return err
	}
	if err := validatePrivateFileInfo(output.logPath, final); err != nil {
		return err
	}
	if err := os.Rename(output.quarantinePath, output.logPath); err != nil {
		return err
	}
	output.quarantinePath = ""
	published, err := os.Lstat(output.logPath)
	if err != nil {
		return err
	}
	return validatePrivateFileInfo(output.logPath, published)
}

func bestEffortStartupError(path string, cause error) {
	if path == "" || cause == nil {
		return
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return
	}
	file, err := openPrivateDestination(filepath.Clean(absolute))
	if err != nil {
		return
	}
	_ = file.Close()
	private := &privatePath{path: filepath.Clean(absolute)}
	message := strings.TrimSpace(cause.Error())
	if len(message) > maxErrorBytes {
		message = message[:maxErrorBytes]
	}
	_ = private.writeAtomic([]byte(message + "\n"))
}

func bestEffortStartupFailureClass(path string, class failureClass) {
	if path == "" {
		return
	}
	if _, found := validFailureClasses[class]; !found {
		return
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return
	}
	file, err := openPrivateDestination(filepath.Clean(absolute))
	if err != nil {
		return
	}
	_ = file.Close()
	private := &privatePath{path: filepath.Clean(absolute)}
	_ = private.writeAtomic([]byte(string(class) + "\n"))
}

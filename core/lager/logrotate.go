//Copyright 2017 Huawei Technologies Co., Ltd
//
//Licensed under the Apache License, Version 2.0 (the "License");
//you may not use this file except in compliance with the License.
//You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
//Unless required by applicable law or agreed to in writing, software
//distributed under the License is distributed on an "AS IS" BASIS,
//WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//See the License for the specific language governing permissions and
//limitations under the License.

// Package lager is the package for lager
package lager

import (
	"archive/zip"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chassis/openlog"
)

var pathReplacer *strings.Replacer

// EscapPath escape path
func EscapPath(msg string) string {
	return pathReplacer.Replace(msg)
}

func removeFile(path string) error {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fileInfo.IsDir() {
		return nil
	}
	err = os.Remove(path)
	if err != nil {
		return err
	}
	return nil
}

func removeExceededFiles(path string, baseFileName string,
	maxKeptCount int, rotateStage string) {
	if maxKeptCount < 0 {
		return
	}
	var pat string
	if rotateStage == "rollover" {
		//rotated file, svc.log.20060102150405000
		pat = fmt.Sprintf(`%s\.[0-9]{1,17}$`, baseFileName)
	} else if rotateStage == "backup" {
		//backup compressed file, svc.log.20060102150405000.zip
		pat = fmt.Sprintf(`%s\.[0-9]{17}\.zip$`, baseFileName)
	} else {
		return
	}
	fileList, err := FilterFileList(path, pat)
	if err != nil {
		Logger.Error(fmt.Sprintf("filepath.Walk() path: %s failed: %s", EscapPath(path), err))
		return
	}
	sort.Strings(fileList)
	if len(fileList) <= maxKeptCount {
		return
	}
	//remove exceeded files, keep file count below maxBackupCount
	for len(fileList) > maxKeptCount {
		filePath := fileList[0]
		err := removeFile(filePath)
		if err != nil {
			Logger.Error(fmt.Sprintf("remove filePath: %s failed: %s", EscapPath(filePath), err))
			break
		}
		//remove the first element of a list
		fileList = append(fileList[:0], fileList[1:]...)
	}
}

//filePath: file full path, like ${_APP_LOG_DIR}/svc.log.1
//fileBaseName: rollover file base name, like svc.log
//replaceTimestamp: whether or not to replace the num. of a rolled file
func compressFile(filePath, fileBaseName string, replaceTimestamp bool) error {
	ifp, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer ifp.Close()

	var zipFilePath string
	if replaceTimestamp {
		//svc.log.1 -> svc.log.20060102150405000.zip
		zipFileBase := fileBaseName + "." + getTimeStamp() + "." + "zip"
		zipFilePath = filepath.Dir(filePath) + "/" + zipFileBase
	} else {
		zipFilePath = filePath + ".zip"
	}
	zipFile, err := os.OpenFile(zipFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0440)
	if err != nil {
		return err
	}
	defer func() {
		err := zipFile.Close()
		if err != nil {
			openlog.Error("can not close log zip file: " + err.Error())
		}
	}()

	zipWriter := zip.NewWriter(zipFile)
	defer zipWriter.Close()

	ofp, err := zipWriter.Create(filepath.Base(filePath))
	if err != nil {
		return err
	}

	_, err = io.Copy(ofp, ifp)
	if err != nil {
		return err
	}

	return nil
}

func shouldRollover(fPath string, MaxFileSize int) bool {
	if MaxFileSize < 0 {
		return false
	}

	fileInfo, err := os.Stat(fPath)
	if err != nil {
		Logger.Error(fmt.Sprintf("state path: %s failed: %s", EscapPath(fPath), err))
		return false
	}

	if fileInfo.Size() > int64(MaxFileSize*1024*1024) {
		return true
	}
	return false
}

func doRollover(fPath string, MaxFileSize int, MaxBackupCount int) {
	if !shouldRollover(fPath, MaxFileSize) {
		return
	}

	timeStamp := getTimeStamp()
	//absolute path
	rotateFile := fPath + "." + timeStamp
	err := CopyFile(fPath, rotateFile)
	if err != nil {
		Logger.Error(fmt.Sprintf("copy path: %s failed: %s", EscapPath(fPath), err))
	}

	//truncate the file
	f, err := os.OpenFile(fPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0640)
	if err != nil {
		Logger.Error(fmt.Sprintf("truncate path: %s failed: %s", EscapPath(fPath), err))
		return
	}
	f.Close()

	//remove exceeded rotate files
	removeExceededFiles(filepath.Dir(fPath), filepath.Base(fPath), MaxBackupCount, "rollover")
}

func doBackup(fPath string, MaxBackupCount int) {
	if MaxBackupCount <= 0 {
		return
	}
	pat := fmt.Sprintf(`%s\.[0-9]{1,17}$`, filepath.Base(fPath))
	rotateFileList, err := FilterFileList(filepath.Dir(fPath), pat)
	if err != nil {
		Logger.Error(fmt.Sprintf("walk path: %s failed: %s", EscapPath(fPath), err))
		return
	}

	for _, file := range rotateFileList {
		var err error
		p := fmt.Sprintf(`%s\.[0-9]{17}$`, filepath.Base(fPath))
		if ret, _ := regexp.MatchString(p, file); ret {
			//svc.log.20060102150405000, not replace Timestamp
			err = compressFile(file, filepath.Base(fPath), false)
		} else {
			//svc.log.1, replace Timestamp
			err = compressFile(file, filepath.Base(fPath), true)
		}
		if err != nil {
			openlog.Error(fmt.Sprintf("compress path: %s failed: %s", EscapPath(file), err))
			continue
		}
		err = removeFile(file)
		if err != nil {
			Logger.Error(fmt.Sprintf("remove path %s failed: %s", EscapPath(file), err))
		}
	}

	//remove exceeded backup files
	removeExceededFiles(filepath.Dir(fPath), filepath.Base(fPath), MaxBackupCount, "backup")
}

func logRotateFile(file string, MaxFileSize int, MaxBackupCount int) {
	defer func() {
		if e := recover(); e != nil {
			Logger.Error(fmt.Sprintf("LogRotate file path: %s catch an exception.", EscapPath(file)))
		}
	}()

	doRollover(file, MaxFileSize, MaxBackupCount)
	doBackup(file, MaxBackupCount)
}

// LogRotate function for log rotate
// path:			where log files need rollover
// MaxFileSize: 		MaxSize of a file before rotate. By M Bytes.
// MaxBackupCount: 	Max counts to keep of a log's backup files.
func LogRotate(path string, MaxFileSize int, MaxBackupCount int) {
	//filter .log .trace files
	defer func() {
		if e := recover(); e != nil {
			Logger.Error(fmt.Sprintf("LogRotate catch an exception, %v", e))
		}
	}()

	pat := `.(\.log|\.trace|\.out)$`
	fileList, err := FilterFileList(path, pat)
	if err != nil {
		Logger.Error(fmt.Sprintf("filepath.Walk() path: %s failed: %s", path, err))
		return
	}

	for _, file := range fileList {
		logRotateFile(file, MaxFileSize, MaxBackupCount)
	}
}

// FilterFileList function for filter file list
//path : where the file will be filtered
//pat  : regexp pattern to filter the matched file
func FilterFileList(path, pat string) ([]string, error) {
	capacity := 10
	//initialize a fileName slice, len=0, cap=10
	fileList := make([]string, 0, capacity)
	err := filepath.Walk(path,
		func(pathName string, f os.FileInfo, e error) error {
			if f == nil {
				return e
			}
			if f.IsDir() {
				return nil
			}
			if pat != "" {
				ret, _ := regexp.MatchString(pat, f.Name())
				if !ret {
					return nil
				}
			}
			fileList = append(fileList, pathName)
			return nil
		})
	return fileList, err
}

// getTimeStamp get time stamp
func getTimeStamp() string {
	now := time.Now().Format("2006.01.02.15.04.05.000")
	timeSlot := strings.Replace(now, ".", "", -1)
	return timeSlot
}

// CopyFile copy file
func CopyFile(srcFile, dstFile string) error {
	input, err := ioutil.ReadFile(srcFile)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(dstFile, input, 0640)
	return err
}

// NewRotateConfig return config
func NewRotateConfig(option *Options) *RotateConfig {
	rc := new(RotateConfig)
	rc.BackupCount = LogBackupCount
	if option.LogBackupCount > 0 {
		rc.BackupCount = option.LogBackupCount
	}
	rc.logFilePath = option.LoggerFile
	rc.logFileDir = filepath.Dir(option.LoggerFile)
	if option.RollingPolicy == RollingPolicySize {
		rc.Size = LogRotateSize
		if option.LogRotateSize > 0 {
			rc.Size = option.LogRotateSize
		}
		rc.CheckCycle = 30 * time.Second
	} else {
		rc.CheckCycle = 24 * time.Hour
		if option.LogRotateDate > 1 {
			rc.CheckCycle = 24 * time.Hour * time.Duration(option.LogRotateDate)
		}
	}
	return rc
}

// Rotators global rotate instance
var Rotators = &rotators{
	logFilePaths: make(map[string]*RotateConfig, 5),
}

type rotators struct {
	logFilePaths map[string]*RotateConfig
	locker       sync.Mutex
}

// RotateConfig rotate config
type RotateConfig struct {
	logFilePath string
	logFileDir  string
	Policy      string
	Size        int
	BackupCount int
	CheckCycle  time.Duration

	RotateDate int
}

// Rotate rotate log
func (r *rotators) Rotate(rc *RotateConfig) {
	r.locker.Lock()
	defer r.locker.Unlock()
	if _, exist := r.logFilePaths[rc.logFileDir]; exist {
		return
	}

	r.logFilePaths[rc.logFilePath] = rc

	go func() {
		openlog.Info("start log rotate task")
		for {
			LogRotate(rc.logFileDir, rc.Size, rc.BackupCount)
			time.Sleep(rc.CheckCycle)
		}
	}()
}

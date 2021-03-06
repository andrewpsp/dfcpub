// Package dfc provides distributed file-based cache with Amazon and Google Cloud backends.
//
// Example run:
//     go test -v -run=rwstress -args -numfiles=10 -cycles=10 -nodel -numops=5
//
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 *
 */
package dfc_test

import (
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NVIDIA/dfcpub/pkg/client/readers"

	"github.com/NVIDIA/dfcpub/dfc"
	"github.com/NVIDIA/dfcpub/pkg/client"
)

const (
	rwdir    = "rwstress"
	fileSize = 1024 * 32 // file size

	// time to sleep if there is no object created yet in milliseconds
	// del time is slightly greater than get one to allow get work faster
	//   than del, so get won't miss objects because del run before get
	getSleep = 5
	delSleep = 10

	rwFileCreated = true
	rwFileExists  = true
	rwFileDeleted = false
	rwRunNormal   = false
	rwRunCleanUp  = true
)

type fileLock struct {
	locked bool
	exists bool
}
type fileLocks struct {
	mtx   sync.Mutex
	files []fileLock
}

var (
	fileNames []string
	filelock  fileLocks

	numLoops   int
	numFiles   int
	putCounter int64
	getCounter int64
	delCounter int64
)

func tryLockFile(idx int) bool {
	filelock.mtx.Lock()
	defer filelock.mtx.Unlock()

	info := filelock.files[idx]
	if info.locked {
		return false
	}

	filelock.files[idx].locked = true
	return true
}

// tryLockNextAvailFile looks for an unlocked file that exists. If such file
// found it returns the id of the file and true. Returns 0 and false otherwise.
// idx is the preferred file id - a starting point to look for a file
func tryLockNextAvailFile(idx int) (int, bool) {
	filelock.mtx.Lock()
	defer filelock.mtx.Unlock()

	info := filelock.files[idx]
	if !info.locked && info.exists {
		filelock.files[idx].locked = true
		return idx, true
	}

	nextIdx := idx + 1
	for nextIdx != idx {
		if nextIdx >= len(fileNames) {
			nextIdx = 0
			continue
		}

		info = filelock.files[nextIdx]
		if !info.locked && info.exists {
			filelock.files[nextIdx].locked = true
			return nextIdx, true
		}

		nextIdx++
	}

	return 0, false
}

// unlockFile unlocks the file and marks if the file exists or not
func unlockFile(idx int, fileExists bool) {
	filelock.mtx.Lock()
	defer filelock.mtx.Unlock()

	filelock.files[idx].locked = false
	filelock.files[idx].exists = fileExists
	return
}

// generates a list of random file names and a buffer to keep random data for filling up files
func generateRandomData(t *testing.T, seed int64, fileCount int) {
	src := rand.NewSource(seed)
	random := rand.New(src)
	fileNames = make([]string, fileCount)

	for i := 0; i < fileCount; i++ {
		fileNames[i] = client.FastRandomFilename(random, fnlen)
	}
}

// rwCanRunAsync limits the number of extra goroutines simultaneously
// running. '+1' is used to take into account the main thread, so if numops
// equals 1 then all operations run one by one non-concurrently
func rwCanRunAsync(currAsyncOps int64, maxAsycOps int) bool {
	return currAsyncOps+1 < int64(maxAsycOps)
}

func rwPutLoop(t *testing.T, fileNames []string, taskGrp *sync.WaitGroup, doneCh chan int) {
	var (
		totalOps int
		prc      int
	)
	errch := make(chan error, 10)
	fileCount := len(fileNames)
	if taskGrp != nil {
		defer taskGrp.Done()
	}

	var wg sync.WaitGroup
	totalCount := fileCount * numLoops
	filesPut := 0
	for i := 0; i < numLoops; i++ {
		for idx := 0; idx < fileCount; idx++ {
			keyname := fmt.Sprintf("%s/%s", rwdir, fileNames[idx])

			// Note: This test depends on the files it creates, so ignore reader type, always use file reader
			r, err := readers.NewFileReader(baseDir, keyname, fileSize, true /* withHash */)
			if err != nil {
				fmt.Fprintf(os.Stdout, "PUT write FAIL: %v\n", err)
				t.Error(err)
				if errch != nil {
					errch <- err
				}
				return
			}
			if ok := tryLockFile(idx); ok {
				n := atomic.LoadInt64(&putCounter)
				if rwCanRunAsync(n, numops) {
					atomic.AddInt64(&putCounter, 1)
					wg.Add(1)
					localIdx := idx
					go func() {
						client.PutAsync(&wg, proxyurl, r, clibucket, keyname, errch, true /* silent */)
						unlockFile(localIdx, rwFileCreated)
						atomic.AddInt64(&putCounter, -1)
					}()
				} else {
					err = client.Put(proxyurl, r, clibucket, keyname, true /* silent */)
					if err != nil {
						errch <- err
					}
					unlockFile(idx, rwFileCreated)
				}
				totalOps++
			}
			filesPut++
			newPrc := 100 * filesPut / totalCount
			if prc != newPrc {
				prc = newPrc
			}
			select {
			case e := <-errch:
				fmt.Printf("PUT failed: %v\n", e.Error())
				t.Fail()
			default:
			}
		}
	}

	wg.Wait()

	// emit signals for DEL and GET loops
	doneCh <- 1
	if !skipdel {
		doneCh <- 1
	}
}

func rwDelLoop(t *testing.T, fileNames []string, taskGrp *sync.WaitGroup, doneCh chan int, doCleanUp bool) {
	done := false
	var totalOps, currIdx int
	errch := make(chan error, 10)
	var wg = &sync.WaitGroup{}

	if taskGrp != nil {
		defer taskGrp.Done()
	}

	for !done {
		if idx, ok := tryLockNextAvailFile(currIdx); ok {
			keyname := fmt.Sprintf("%s/%s", rwdir, fileNames[idx])
			n := atomic.LoadInt64(&delCounter)
			if rwCanRunAsync(n, numops) {
				atomic.AddInt64(&delCounter, 1)
				wg.Add(1)
				localIdx := idx
				go func() {
					client.Del(proxyurl, clibucket, keyname, wg, errch, true)
					unlockFile(localIdx, rwFileDeleted)
					atomic.AddInt64(&delCounter, -1)
				}()
			} else {
				client.Del(proxyurl, clibucket, keyname, nil, errch, true)
				unlockFile(idx, rwFileDeleted)
			}

			currIdx = idx + 1
			if currIdx >= len(fileNames) {
				currIdx = 0
			}
			totalOps++
		} else {
			if doCleanUp {
				break
			}
			time.Sleep(delSleep * time.Millisecond)
		}

		select {
		case <-doneCh:
			done = true
		case e := <-errch:
			fmt.Printf("DEL failed: %v\n", e.Error())
			t.Fail()
		default:
		}
	}
	wg.Wait()
}

func rwGetLoop(t *testing.T, fileNames []string, taskGrp *sync.WaitGroup, doneCh chan int) {
	done := false
	var currIdx, totalOps int
	errch := make(chan error, 10)
	var wg = &sync.WaitGroup{}

	if taskGrp != nil {
		defer taskGrp.Done()
	}

	for !done {
		if idx, ok := tryLockNextAvailFile(currIdx); ok {
			keyname := fmt.Sprintf("%s/%s", rwdir, fileNames[idx])
			n := atomic.LoadInt64(&getCounter)
			if rwCanRunAsync(n, numops) {
				atomic.AddInt64(&getCounter, 1)
				wg.Add(1)
				localIdx := idx
				go func() {
					client.Get(proxyurl, clibucket, keyname, wg, errch, true, false)
					unlockFile(localIdx, rwFileExists)
					atomic.AddInt64(&getCounter, -1)
				}()
			} else {
				client.Get(proxyurl, clibucket, keyname, nil, errch, true, false)
				unlockFile(idx, rwFileExists)
			}
			currIdx = idx + 1
			if currIdx >= len(fileNames) {
				currIdx = 0
			}
			totalOps++
		} else {
			time.Sleep(getSleep * time.Millisecond)
		}

		select {
		case <-doneCh:
			done = true
		case e := <-errch:
			fmt.Printf("GET failed: %v\n", e.Error())
			t.Fail()
		default:
		}
	}

	wg.Wait()
}

func rwstress(t *testing.T) {
	if err := dfc.CreateDir(fmt.Sprintf("%s/%s", baseDir, rwdir)); err != nil {
		t.Fatalf("Failed to create dir %s/%s, err: %v", baseDir, rwdir, err)
	}

	created := createLocalBucketIfNotExists(t, proxyurl, clibucket)
	filelock.files = make([]fileLock, numFiles, numFiles)

	generateRandomData(t, baseseed+10000, numFiles)

	var wg sync.WaitGroup
	doneCh := make(chan int, 2)
	wg.Add(1)
	go rwPutLoop(t, fileNames, &wg, doneCh)
	wg.Add(1)
	go rwGetLoop(t, fileNames, &wg, doneCh)
	if !skipdel {
		wg.Add(1)
		go rwDelLoop(t, fileNames, &wg, doneCh, rwRunNormal)
	}

	wg.Wait()
	rwDelLoop(t, fileNames, nil, doneCh, rwRunCleanUp)
	rwstressCleanup(t)

	if created {
		if err := client.DestroyLocalBucket(proxyurl, clibucket); err != nil {
			t.Errorf("Failed to delete local bucket: %v", err)
		}
	}
}

func rwstressCleanup(t *testing.T) {
	fileDir := fmt.Sprintf("%s/%s", baseDir, rwdir)

	for _, fileName := range fileNames {
		e := os.Remove(fmt.Sprintf("%s/%s", fileDir, fileName))
		if e != nil {
			fmt.Printf("Failed to remove file %s: %v\n", fileName, e)
			t.Error(e)
		}
	}
}

func TestRWStress(t *testing.T) {
	numFiles = 25
	numLoops = 8

	rwstress(t)
}

// Test_rwstress runs delete, put, and get operations in a loop
// Since PUT is the longest operation, PUT loop runs the defined number
//    of cycles and emits a done signal at the end. Both GET and DEL run
//    endlessly until PUT loop emits the done signal
// If -nodel is on then the test runs only PUT and GET in a loop and after they
//    complete the test runs DEL loop to clean up
// If the test runs asynchronusly all three kinds of operations then after the
//    test finishes it executes extra loop to delete all files
func Test_rwstress(t *testing.T) {
	if testing.Short() {
		t.Skip("Long run only")
	}

	if err := client.Tcping(proxyurl); err != nil {
		tlogf("%s: %v\n", proxyurl, err)
		os.Exit(1)
	}

	numLoops = cycles
	numFiles = numfiles
	rwstress(t)
}

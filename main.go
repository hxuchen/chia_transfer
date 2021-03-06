/**
 _*_ @Author: IronHuang _*_
 _*_ @blog:https://www.dvpos.com/ _*_
 _*_ @Date: 2021/5/24 下午9:46 _*_
**/

package main

import (
	"errors"
	"fmt"
	"github.com/filecoin-project/lotus/lib/lotuslog"
	logging "github.com/ipfs/go-log"
	"gopkg.in/yaml.v2"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"os/user"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var log = logging.Logger("transfer")
var stop = false
var onWorkingSrc = OnWorkingSrc{
	onWorkingSrcMap: make(map[string]struct{}),
	OWSLock:         new(sync.Mutex),
}

var dstPathSingleton = DstPaths{
	DstPathMap: make(map[string]bool),
	DLock:      new(sync.Mutex),
}

const (
	StatusOnWorking = "StatusOnWorking"
	StatusOnFree    = "StatusOnFree"
)

type DstPaths struct {
	DstPathMap map[string]bool // if true means on working
	DLock      *sync.Mutex
}

type OnWorkingSrc struct {
	onWorkingSrcMap map[string]struct{}
	OWSLock         *sync.Mutex
}

type Config struct {
	MiddleTmps []string
	FinalDirs  []string
}

func main() {
	lotuslog.SetupLogLevels()
	config, err := loadConfig()
	if err != nil {
		log.Error(err)
		return
	}
	// init dst path map
	dstPathSingleton := initDstPathSingleton(config)

	// listen signal
	stopSignal := make(chan os.Signal, 2)
	signal.Notify(stopSignal, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case <-stopSignal:
			log.Warn("stopped by signal,will exit the process soon")
			stop = true
		}
	}()

	threadChan := make(chan struct{}, len(dstPathSingleton.DstPathMap))
	roundTimes := 1
	for {
		log.Debugf("round NO.%d", roundTimes)
		if stop {
			log.Warn("stop by signal,waiting all working task stop")
			for {
				workingNum := 0
				for k, f := range dstPathSingleton.DstPathMap {
					if f {
						log.Info("%s is still working", k)
						workingNum++
					}
				}
				if workingNum == 0 {
					log.Warn("all working task stopped")
					close(threadChan)
					return
				}
				time.Sleep(time.Second * 5)
			}
		}

		for _, mid := range config.MiddleTmps {
			if len(config.MiddleTmps) == 0 {
				break
			}
			mp := mid
			err := filepath.Walk(mp, func(path string, PathInfo os.FileInfo, err error) error {
				singlePath := path
				info := PathInfo
				if info == nil {
					return nil
				}

				// if src is copying, skip
				if _, ok := onWorkingSrc.onWorkingSrcMap[info.Name()]; ok {
					return nil
				}

				if info.IsDir() || !info.Mode().IsRegular() {
					return nil
				}

				// if not end with ".plot", skip
				NameSplit := strings.Split(info.Name(), ".")
				if NameSplit[len(NameSplit)-1] != "plot" {
					return nil
				}

				// try to get on free dst
				found := false
				for key, value := range dstPathSingleton.DstPathMap {
					p := key
					onWorking := value
					if key == "" {
						continue
					}
					// if not onWorking,chose this p as dst
					if !onWorking {
						// has enough space available or not
						var stat = new(syscall.Statfs_t)
						_ = syscall.Statfs(p, stat)
						if stat.Bavail*uint64(stat.Bsize) < uint64(info.Size()) {
							continue
						}
						// make full dst path
						fullDstPath := fmt.Sprintf("%s/%s", p, info.Name())
						select {
						case threadChan <- struct{}{}:
							// got one thread
							// start copy
							dstPathSingleton.DstPathMap[p] = true
							onWorkingSrc.onWorkingSrcMap[info.Name()] = struct{}{}
							go startCopy(singlePath, fullDstPath, p, info.Name(), threadChan)
							found = true
						default:
						}
					}
					if found {
						break
					}
				}
				return err
			})

			if err != nil {
				log.Error(err)
				return
			}

		}

		// wait 5 minute
		waitingForNextRound()
		roundTimes++
	}
}

func waitingForNextRound() {
	log.Info("wait 5 minutes, before next round")
	for i := 0; i < 150; i++ {
		if stop {
			break
		}
		time.Sleep(time.Second * 2)
	}
}

func startCopy(src, dst, dstDir, srcName string, threadChan chan struct{}) {
	log.Infof("start copying from src %s to dst %s", src, dst)
	err := myCopy(src, dst)
	if err != nil {
		os.Remove(dst)
		log.Errorf("error:%s, when copy from %s to %s", err.Error(), src, dst)
	} else {
		// confirm is equal
		if isEqualFile(src, dst) {
			os.Remove(src)
			log.Infof("copy done: from %s to %s", src, dst)
		} else {
			os.Remove(dst)
			log.Errorf("not equal between %s and %s,will copy again later", src, dst)
		}
	}

	// change status
	dstPathSingleton.DLock.Lock()
	dstPathSingleton.DstPathMap[dstDir] = false
	dstPathSingleton.DLock.Unlock()
	onWorkingSrc.OWSLock.Lock()
	delete(onWorkingSrc.onWorkingSrcMap, srcName)
	onWorkingSrc.OWSLock.Unlock()
	_ = <-threadChan
	return
}

func loadConfig() (*Config, error) {
	currentUser, err := user.Current()
	if err != nil {
		return nil, err
	}
	raw, err := ioutil.ReadFile(path.Join(currentUser.HomeDir, "chia_transfer.yaml"))
	if err != nil {
		return nil, err
	}
	config := Config{}
	err = yaml.Unmarshal(raw, &config)
	if err != nil {
		return nil, err
	}
	log.Info(config)
	if len(config.FinalDirs) <= 0 || len(config.MiddleTmps) <= 0 {
		return nil, errors.New("len dirs error")
	}

	err = checkPathDoubledAndExisted(&config)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

func initDstPathSingleton(cfg *Config) DstPaths {
	for _, v := range cfg.FinalDirs {
		dstPathSingleton.DstPathMap[v] = false
	}
	return dstPathSingleton
}

func myCopy(src, dst string) (err error) {
	const BufferSize = 1 * 1024 * 1024
	buf := make([]byte, BufferSize)

	sourceFileStat, err := os.Stat(src)
	if err != nil {
		return err
	}

	if !sourceFileStat.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}

	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		err2 := source.Close()
		if err2 != nil && err == nil {
			err = err2
		}
	}()

	if err != nil {
		return err
	}
	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		err2 := destination.Close()
		if err2 != nil && err == nil {
			err = err2
		}
	}()

	for {
		if stop {
			return errors.New("StoppedBySyscall")
		}

		n, err := source.Read(buf)
		if err != nil && err != io.EOF {
			return err
		}
		if n == 0 {
			break
		}

		// speed limit
		//if singleThreadMBPS != 0 {
		//	sleepTime := 1000000 / int64(singleThreadMBPS)
		//	time.Sleep(time.Microsecond * time.Duration(sleepTime))
		//}

		if _, err := destination.Write(buf[:n]); err != nil {
			return err
		}
	}
	return
}

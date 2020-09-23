/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package api_load

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

import (
	"github.com/apache/dubbo-go/common/logger"
	"github.com/dubbogo/dubbo-go-proxy/pkg/model"
	"github.com/dubbogo/dubbo-go-proxy/pkg/service"
)

type ApiLoadType string

const (
	File  ApiLoadType = "file"
	Nacos ApiLoadType = "nacos"
)

type ApiManager struct {
	mergeLock *sync.RWMutex
	// rate limiter
	limiter         *time.Ticker
	rateLimiterTime time.Duration
	mergeTask       chan struct{}
	// store apiLoaders
	ApiLoadTypeMap map[ApiLoadType]ApiLoader
	ads            service.ApiDiscoveryService
}

func NewApiManager(rateLimiterTime time.Duration, ads service.ApiDiscoveryService) *ApiManager {
	if rateLimiterTime < time.Millisecond*50 {
		rateLimiterTime = time.Millisecond * 50
	}
	return &ApiManager{
		ApiLoadTypeMap:  make(map[ApiLoadType]ApiLoader, 8),
		mergeTask:       make(chan struct{}, 1),
		limiter:         time.NewTicker(rateLimiterTime),
		rateLimiterTime: rateLimiterTime,
		mergeLock:       &sync.RWMutex{},
		ads:             ads,
	}
}

// add apiLoader by ApiLoadType
func (al *ApiManager) AddApiLoader(config model.ApiConfig) {
	if config.File != nil {
		al.ApiLoadTypeMap[File] = NewFileApiLoader(WithFilePath(config.File.FileApiConfPath))
	}
	if config.Nacos != nil {
		al.ApiLoadTypeMap[Nacos] = NewNacosApiLoader(WithNacosAddress(config.Nacos.Address))
	}
}

// nolint
func (al *ApiManager) GetApiLoad(apiLoadType ApiLoadType) (ApiLoader, error) {
	if apiLoader, ok := al.ApiLoadTypeMap[apiLoadType]; ok {
		return apiLoader, nil
	}
	return nil, errors.New(fmt.Sprintf("can't load apiLoader for :%s", apiLoadType))
}

// start to load apis using apiLoaders stored in ApiLoadTypeMap
func (al *ApiManager) StartLoadApi() error {
	for _, loader := range al.ApiLoadTypeMap {
		err := loader.InitLoad()
		if err != nil {
			logger.Warn("proxy init api error:%v", err)
			break
		}
	}

	if al.limiter == nil {
		return errors.New("proxy won't hot load api since limiter is null.")
	}

	for _, loader := range al.ApiLoadTypeMap {
		changeNotifier, err := loader.HotLoad()
		if err != nil {
			logger.Warn("proxy hot load api error:%v", err)
			break
		}

		go func() {
			for {
				select {
				case _, ok := <-changeNotifier:
					if !ok {
						logger.Debug("changeNotifier of apiloader was closed!")
						return
					}
					al.AddMergeTask()
					break
				}
			}
		}()
	}
	return nil
}

// store a message to mergeTask to notify calling DoMergeApiTask
func (al *ApiManager) AddMergeTask() error {
	select {
	case al.mergeTask <- struct{}{}:
		logger.Debug("added a merge task, waiting to merge api.")
		break
	case <-time.After(5 * time.Second):
		logger.Errorf("add merge task fail:wait timeout.")
		break
	}
	return nil
}

// to merge apis to store in ads.Notice that limiter will limit frequency of merging.
func (al *ApiManager) SelectMergeApiTask() (err error) {
	for {
		select {
		case <-al.limiter.C:
			if len(al.mergeTask) > 0 {
				_, err = al.DoMergeApiTask()
				if err != nil {
					logger.Warnf("error merge api task:%v", err)
				}
			}
			//al.limiter.Reset(time.Second)
			break
		default:
			time.Sleep(al.rateLimiterTime / 10)
			break
		}
	}
	return
}

// merge apis
func (al *ApiManager) DoMergeApiTask() (skip bool, err error) {
	al.mergeLock.Lock()
	defer al.mergeLock.Unlock()
	wait := time.After(time.Millisecond * 50)
	select {
	case <-wait:
		logger.Debug("merge api task is too frequent.")
		skip = true
		return
	case <-al.mergeTask:
		// If apiLoadType is File,then try covering it's apis using other's apis from registry center
		multiApisMerged := make(map[string]model.Api, 8)
		var sortedApiLoader []int
		sortedApiLoaderMap := make(map[int]ApiLoadType, len(al.ApiLoadTypeMap))
		for apiLoadType, loader := range al.ApiLoadTypeMap {
			sortedApiLoader = append(sortedApiLoader, loader.GetPrior())
			sortedApiLoaderMap[loader.GetPrior()] = apiLoadType
		}

		sort.Ints(sortedApiLoader)
		for _, sortNo := range sortedApiLoader {
			loadType := sortedApiLoaderMap[sortNo]
			apiLoader := al.ApiLoadTypeMap[loadType]
			var apiConfigs []model.Api
			apiConfigs, err = apiLoader.GetLoadedApiConfigs()
			if err != nil {
				logger.Error("get file apis error:%v", err)
				return
			} else {
				for _, fleApiConfig := range apiConfigs {
					if fleApiConfig.Status != model.Up {
						continue
					}
					multiApisMerged[al.buildApiID(fleApiConfig)] = fleApiConfig
				}
			}
		}

		var totalApis []model.Api
		for _, api := range multiApisMerged {
			totalApis = append(totalApis, api)
		}
		err = al.ads.RemoveAllApi()
		if err != nil {
			logger.Errorf("remove all older apis error:%v", err)
			return
		}
		err = al.add2ApiDiscoveryService(totalApis)
		if err != nil {
			logger.Errorf("add newer apis error:%v", err)
			return
		}
		return
	}
}

// add merged apis to ads
func (al *ApiManager) add2ApiDiscoveryService(apis []model.Api) error {
	for _, api := range apis {
		j, _ := json.Marshal(api)
		_, err := al.ads.AddApi(*service.NewDiscoveryRequest(j))
		if err != nil {
			logger.Errorf("error add api:%s", j)
			return err
		}
	}
	return nil
}

// nolint
func (al *ApiManager) buildApiID(api model.Api) string {
	return fmt.Sprintf("name:%s,ITypeStr:%s,OTypeStr:%s,Method:%s",
		api.Name, api.ITypeStr, api.OTypeStr, api.Method)
}
/*
Copyright 2021 The KubeVela Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package usecase

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/oam-dev/terraform-controller/api/types"
	"github.com/oam-dev/terraform-controller/api/v1beta1"
	"github.com/pkg/errors"
	v12 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/pkg/apiserver/clients"
	"github.com/oam-dev/kubevela/pkg/apiserver/datastore"
	"github.com/oam-dev/kubevela/pkg/apiserver/log"
	"github.com/oam-dev/kubevela/pkg/apiserver/model"
	apis "github.com/oam-dev/kubevela/pkg/apiserver/rest/apis/v1"
	utils2 "github.com/oam-dev/kubevela/pkg/apiserver/rest/utils"
	"github.com/oam-dev/kubevela/pkg/apiserver/rest/utils/bcode"
	"github.com/oam-dev/kubevela/pkg/cloudprovider"
	"github.com/oam-dev/kubevela/pkg/multicluster"
	"github.com/oam-dev/kubevela/pkg/utils"
	"github.com/oam-dev/kubevela/pkg/utils/util"
)

// ClusterUsecase cluster manage
type ClusterUsecase interface {
	ListKubeClusters(context.Context, string, int, int) (*apis.ListClusterResponse, error)
	CreateKubeCluster(context.Context, apis.CreateClusterRequest) (*apis.ClusterBase, error)
	GetKubeCluster(context.Context, string) (*apis.DetailClusterResponse, error)
	ModifyKubeCluster(context.Context, apis.CreateClusterRequest, string) (*apis.ClusterBase, error)
	DeleteKubeCluster(context.Context, string) (*apis.ClusterBase, error)

	CreateClusterNamespace(context.Context, string, apis.CreateClusterNamespaceRequest) (*apis.CreateClusterNamespaceResponse, error)

	ListCloudClusters(context.Context, string, apis.AccessKeyRequest, int, int) (*apis.ListCloudClusterResponse, error)
	ConnectCloudCluster(context.Context, string, apis.ConnectCloudClusterRequest) (*apis.ClusterBase, error)
	CreateCloudCluster(context.Context, string, apis.CreateCloudClusterRequest) (*apis.CreateCloudClusterResponse, error)
	GetCloudClusterCreationStatus(context.Context, string, string) (*apis.CreateCloudClusterResponse, error)
	ListCloudClusterCreation(context.Context, string) (*apis.ListCloudClusterCreationResponse, error)
	DeleteCloudClusterCreation(context.Context, string, string) (*apis.CreateCloudClusterResponse, error)
}

type clusterUsecaseImpl struct {
	ds        datastore.DataStore
	caches    map[string]*utils2.MemoryCache
	k8sClient client.Client
}

// NewClusterUsecase new cluster usecase
func NewClusterUsecase(ds datastore.DataStore) ClusterUsecase {
	k8sClient, err := clients.GetKubeClient()
	if err != nil {
		log.Logger.Fatalf("get k8sClient failure: %s", err.Error())
	}
	c := &clusterUsecaseImpl{ds: ds, k8sClient: k8sClient, caches: make(map[string]*utils2.MemoryCache)}
	if err = c.preAddLocalCluster(context.Background()); err != nil {
		log.Logger.Fatalf("preAdd local cluster failure: %s", err.Error())
	}
	return c
}

func _getClusterFromDataStore(ctx context.Context, ds datastore.DataStore, clusterName string) (*model.Cluster, error) {
	cluster := &model.Cluster{Name: clusterName}
	if err := ds.Get(ctx, cluster); err != nil {
		return nil, err
	}
	return cluster, nil
}

func (c *clusterUsecaseImpl) getClusterFromDataStore(ctx context.Context, clusterName string) (*model.Cluster, error) {
	return _getClusterFromDataStore(ctx, c.ds, clusterName)
}

func (c *clusterUsecaseImpl) rollbackAddedClusterInDataStore(ctx context.Context, cluster *model.Cluster) {
	if e := c.ds.Delete(ctx, cluster); e != nil {
		log.Logger.Errorf("failed to rollback added cluster %s in data store: %s", cluster.Name, e.Error())
	}
}

func (c *clusterUsecaseImpl) rollbackDeletedClusterInDataStore(ctx context.Context, cluster *model.Cluster) {
	if e := c.ds.Add(ctx, cluster); e != nil {
		log.Logger.Errorf("failed to rollback deleted cluster %s in data store: %s", cluster.Name, e.Error())
	}
}

func (c *clusterUsecaseImpl) rollbackJoinedKubeCluster(ctx context.Context, cluster *model.Cluster) {
	if e := multicluster.DetachCluster(ctx, c.k8sClient, cluster.Name); e != nil {
		log.Logger.Errorf("failed to rollback joined cluster %s in kubevela: %s", cluster.Name, e.Error())
	}
}

func (c *clusterUsecaseImpl) rollbackDetachedKubeCluster(ctx context.Context, cluster *model.Cluster) {
	if _, e := joinClusterByKubeConfigString(ctx, c.k8sClient, cluster.Name, cluster.KubeConfig); e != nil {
		log.Logger.Errorf("failed to rollback detached cluster %s in kubevela: %s", cluster.Name, e.Error())
	}
}

func (c *clusterUsecaseImpl) preAddLocalCluster(ctx context.Context) error {
	cfg, err := clients.GetKubeConfig()
	if err != nil {
		return err
	}
	localCluster := &model.Cluster{
		Name:         multicluster.ClusterLocalName,
		Description:  "The hub manage cluster where KubeVela runs on.",
		Status:       model.ClusterStatusHealthy,
		APIServerURL: cfg.Host + cfg.APIPath,
	}
	if err = c.ds.Get(ctx, localCluster); err != nil {
		// no local cluster in datastore
		if errors.Is(err, datastore.ErrRecordNotExist) {
			if err = c.ds.Add(ctx, localCluster); err != nil {
				// local cluster already added in datastore
				if errors.Is(err, datastore.ErrRecordExist) {
					return nil
				}
				return err
			}
			return nil
		}
		return err
	}
	if localCluster.CreateTime.Before(model.LocalClusterCreatedTime) {
		localCluster.CreateTime = model.LocalClusterCreatedTime
		if err = c.ds.Put(ctx, localCluster); err != nil {
			return err
		}
	}
	return nil
}

func (c *clusterUsecaseImpl) ListKubeClusters(ctx context.Context, query string, page int, pageSize int) (*apis.ListClusterResponse, error) {
	var queries []datastore.FuzzyQueryOption
	if query != "" {
		queries = append(queries, datastore.FuzzyQueryOption{Key: "name", Query: query})
	}
	fo := datastore.FilterOptions{Queries: queries}
	clusters, err := c.ds.List(ctx, &model.Cluster{}, &datastore.ListOptions{
		Page:          page,
		PageSize:      pageSize,
		SortBy:        []datastore.SortOption{{Key: "model.createTime", Order: datastore.SortOrderDescending}},
		FilterOptions: fo,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to list cluster with query %s in data store", query)
	}
	resp := &apis.ListClusterResponse{
		Clusters: []apis.ClusterBase{},
	}
	for _, raw := range clusters {
		cluster, ok := raw.(*model.Cluster)
		if ok {
			resp.Clusters = append(resp.Clusters, *newClusterBaseFromCluster(cluster))
		}
	}
	total, err := c.ds.Count(ctx, &model.Cluster{}, &fo)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to count cluster with query %s in data store", query)
	}
	resp.Total = total
	return resp, nil
}

func joinClusterByKubeConfigString(ctx context.Context, k8sClient client.Client, clusterName string, kubeConfig string) (string, error) {
	tmpFileName := fmt.Sprintf("/tmp/cluster-secret-%s-%d.kubeconfig", utils.RandomString(8), time.Now().UnixNano())
	if err := ioutil.WriteFile(tmpFileName, []byte(kubeConfig), 0600); err != nil {
		return "", errors.Wrapf(err, "failed to write kubeconfig to temp file %s", tmpFileName)
	}
	defer func() {
		_ = os.Remove(tmpFileName)
	}()
	cluster, err := multicluster.JoinClusterByKubeConfig(ctx, k8sClient, tmpFileName, clusterName)
	if err != nil {
		if errors.Is(err, multicluster.ErrClusterExists) {
			return "", bcode.ErrClusterExistsInKubernetes
		}
		return "", errors.Wrapf(err, "failed to join cluster")
	}
	return cluster.Server, nil
}

func createClusterModelFromRequest(req apis.CreateClusterRequest, oldCluster *model.Cluster) (newCluster *model.Cluster) {
	if oldCluster != nil {
		newCluster = oldCluster.DeepCopy()
	} else {
		newCluster = &model.Cluster{}
	}
	newCluster.Name = req.Name
	newCluster.Alias = req.Alias
	newCluster.Description = req.Description
	newCluster.Icon = req.Icon
	newCluster.Labels = req.Labels
	newCluster.KubeConfig = req.KubeConfig
	newCluster.KubeConfigSecret = req.KubeConfigSecret
	newCluster.DashboardURL = req.DashboardURL
	return newCluster
}

func (c *clusterUsecaseImpl) createKubeCluster(ctx context.Context, req apis.CreateClusterRequest, providerCluster *cloudprovider.CloudCluster) (*apis.ClusterBase, error) {
	var err error
	cluster := createClusterModelFromRequest(req, nil)
	if cluster.Name == multicluster.ClusterLocalName {
		return nil, bcode.ErrLocalClusterReserved
	}
	t := time.Now()
	cluster.SetCreateTime(t)
	cluster.SetUpdateTime(t)
	if providerCluster != nil {
		cluster.Provider = model.ProviderInfo{
			Provider:    providerCluster.Provider,
			ClusterName: providerCluster.Name,
			ClusterID:   providerCluster.ID,
			Zone:        providerCluster.Zone,
			ZoneID:      providerCluster.ZoneID,
			RegionID:    providerCluster.RegionID,
			VpcID:       providerCluster.VpcID,
			Labels:      providerCluster.Labels,
		}
		cluster.DashboardURL = providerCluster.DashBoardURL
	}
	if err = c.ds.Get(ctx, cluster); err == nil {
		return nil, bcode.ErrClusterAlreadyExistInDataStore
	} else if !errors.Is(err, datastore.ErrRecordNotExist) {
		return nil, err
	}
	if req.KubeConfig != "" {
		cluster.APIServerURL, err = joinClusterByKubeConfigString(ctx, c.k8sClient, req.Name, req.KubeConfig)
		if err != nil {
			return nil, err
		}
		c.setClusterStatusAndResourceInfo(ctx, cluster)
		if err = c.ds.Add(ctx, cluster); err != nil {
			c.rollbackJoinedKubeCluster(ctx, cluster)
			if errors.Is(err, datastore.ErrRecordExist) {
				return nil, bcode.ErrClusterAlreadyExistInDataStore
			}
			return nil, err
		}
		return newClusterBaseFromCluster(cluster), nil
	}
	if req.KubeConfigSecret != "" {
		return nil, bcode.ErrKubeConfigSecretNotSupport
	}
	return nil, bcode.ErrKubeConfigAndSecretIsNotSet
}

func (c *clusterUsecaseImpl) CreateKubeCluster(ctx context.Context, req apis.CreateClusterRequest) (*apis.ClusterBase, error) {
	return c.createKubeCluster(ctx, req, nil)
}

func (c *clusterUsecaseImpl) GetKubeCluster(ctx context.Context, clusterName string) (*apis.DetailClusterResponse, error) {
	cluster, err := c.getClusterFromDataStore(ctx, clusterName)
	if err != nil {
		if errors.Is(err, datastore.ErrRecordNotExist) {
			return nil, bcode.ErrClusterNotFoundInDataStore
		}
		return nil, errors.Wrapf(err, "failed to found cluster %s in data store", clusterName)
	}
	resourceInfo := c.setClusterStatusAndResourceInfo(ctx, cluster)
	if err = c.ds.Put(ctx, cluster); err != nil {
		return nil, errors.Wrapf(err, "failed to update cluster %s status info", clusterName)
	}
	return &apis.DetailClusterResponse{
		Cluster:      *cluster,
		ResourceInfo: resourceInfo,
	}, nil
}

func (c *clusterUsecaseImpl) ModifyKubeCluster(ctx context.Context, req apis.CreateClusterRequest, clusterName string) (*apis.ClusterBase, error) {
	oldCluster, err := c.getClusterFromDataStore(ctx, clusterName)
	if err != nil {
		if errors.Is(err, datastore.ErrRecordNotExist) {
			return nil, bcode.ErrClusterNotFoundInDataStore
		}
		return nil, errors.Wrapf(err, "failed to found cluster %s in data store", clusterName)
	}

	newCluster := createClusterModelFromRequest(req, oldCluster)
	newCluster.SetUpdateTime(time.Now())
	if oldCluster.Name != newCluster.Name || oldCluster.KubeConfig != newCluster.KubeConfig || oldCluster.KubeConfigSecret != newCluster.KubeConfigSecret {
		if clusterName == multicluster.ClusterLocalName || newCluster.Name == multicluster.ClusterLocalName {
			return nil, bcode.ErrLocalClusterImmutable
		}
		if newCluster.KubeConfig == "" && newCluster.KubeConfigSecret != "" {
			return nil, bcode.ErrKubeConfigSecretNotSupport
		}
		newClusterTempName := newCluster.Name + "_tmp_" + utils.RandomString(8)
		newCluster.APIServerURL, err = joinClusterByKubeConfigString(ctx, c.k8sClient, newCluster.Name, newCluster.KubeConfig)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to join new cluster %s", newCluster.Name)
		}
		c.setClusterStatusAndResourceInfo(ctx, newCluster)
		rollbackTempCluster := func() {
			rollBackCluster := newCluster.DeepCopy()
			rollBackCluster.Name = newClusterTempName
			c.rollbackJoinedKubeCluster(ctx, rollBackCluster)
		}
		if err = multicluster.DetachCluster(ctx, c.k8sClient, oldCluster.Name); err != nil {
			rollbackTempCluster()
			return nil, errors.Wrapf(err, "failed to detach old cluster %s", oldCluster.Name)
		}
		if err = c.ds.Delete(ctx, oldCluster); err != nil {
			rollbackTempCluster()
			c.rollbackDetachedKubeCluster(ctx, oldCluster)
			if errors.Is(err, datastore.ErrRecordNotExist) {
				return nil, bcode.ErrClusterNotFoundInDataStore
			}
			return nil, errors.Wrapf(err, "failed to delete old cluster %s from datastore", oldCluster.Name)
		}
		if err = c.ds.Add(ctx, newCluster); err != nil {
			rollbackTempCluster()
			c.rollbackDetachedKubeCluster(ctx, oldCluster)
			c.rollbackDeletedClusterInDataStore(ctx, oldCluster)
			if errors.Is(err, datastore.ErrRecordExist) {
				return nil, bcode.ErrClusterAlreadyExistInDataStore
			}
			return nil, errors.Wrapf(err, "failed to add new cluster %s to datastore", newCluster.Name)
		}
		if err = multicluster.RenameCluster(ctx, c.k8sClient, newClusterTempName, newCluster.Name); err != nil {
			rollbackTempCluster()
			c.rollbackDetachedKubeCluster(ctx, oldCluster)
			c.rollbackDeletedClusterInDataStore(ctx, oldCluster)
			c.rollbackAddedClusterInDataStore(ctx, newCluster)
			return nil, errors.Wrapf(err, "failed to rename temporary cluster %s to %s", newClusterTempName, newCluster.Name)
		}
	} else {
		newCluster.Status = oldCluster.Status
		newCluster.Reason = oldCluster.Reason
		if err = c.ds.Put(ctx, newCluster); err != nil {
			return nil, errors.Wrapf(err, "failed to update cluster %s", newCluster.Name)
		}
	}
	return newClusterBaseFromCluster(newCluster), nil
}

func (c *clusterUsecaseImpl) DeleteKubeCluster(ctx context.Context, clusterName string) (*apis.ClusterBase, error) {
	if clusterName == multicluster.ClusterLocalName {
		return nil, bcode.ErrLocalClusterImmutable
	}
	cluster, err := c.getClusterFromDataStore(ctx, clusterName)
	if err != nil {
		if errors.Is(err, datastore.ErrRecordNotExist) {
			return nil, bcode.ErrClusterNotFoundInDataStore
		}
		return nil, errors.Wrapf(err, "failed to found cluster %s in data store", clusterName)
	}
	if err = c.ds.Delete(ctx, cluster); err != nil {
		if errors.Is(err, datastore.ErrRecordNotExist) {
			return nil, bcode.ErrClusterNotFoundInDataStore
		}
		return nil, errors.Wrapf(err, "failed to delete cluster %s in data store", clusterName)
	}
	if err = multicluster.DetachCluster(ctx, c.k8sClient, clusterName); err != nil {
		c.rollbackDeletedClusterInDataStore(ctx, cluster)
		return nil, errors.Wrapf(err, "failed to delete cluster %s in kubernetes", clusterName)
	}
	return newClusterBaseFromCluster(cluster), nil
}

func (c *clusterUsecaseImpl) CreateClusterNamespace(ctx context.Context, clusterName string, req apis.CreateClusterNamespaceRequest) (*apis.CreateClusterNamespaceResponse, error) {
	_, err := c.getClusterFromDataStore(ctx, clusterName)
	if err != nil {
		if errors.Is(err, datastore.ErrRecordNotExist) {
			return nil, bcode.ErrClusterNotFoundInDataStore
		}
		return nil, errors.Wrapf(err, "failed to found cluster %s in data store", clusterName)
	}
	ns := &v12.Namespace{}
	ns.Name = req.Namespace
	if err = c.k8sClient.Create(multicluster.ContextWithClusterName(ctx, clusterName), ns); err != nil {
		if kerrors.IsAlreadyExists(err) {
			return &apis.CreateClusterNamespaceResponse{Exists: true}, nil
		}
		if kerrors.IsForbidden(err) {
			return nil, bcode.ErrClusterCreateNamespaceNoPermission
		}
		return nil, errors.Wrapf(err, "failed to create namespace %s in cluster %s", req.Namespace, clusterName)
	}
	return &apis.CreateClusterNamespaceResponse{Exists: false}, nil
}

func (c *clusterUsecaseImpl) setClusterStatusAndResourceInfo(ctx context.Context, cluster *model.Cluster) apis.ClusterResourceInfo {
	resourceInfo, err := c.getClusterResourceInfoFromK8s(ctx, cluster.Name)
	if err != nil {
		cluster.Status = model.ClusterStatusUnhealthy
		cluster.Reason = fmt.Sprintf("Failed to get cluster resource info: %s", err.Error())
	} else {
		cluster.Status = model.ClusterStatusHealthy
		cluster.Reason = ""
	}
	return resourceInfo
}

func (c *clusterUsecaseImpl) getClusterResourceInfoCacheKey(clusterName string) string {
	return "cluster-resource-info::" + clusterName
}

func (c *clusterUsecaseImpl) getClusterResourceInfoFromK8s(ctx context.Context, clusterName string) (apis.ClusterResourceInfo, error) {
	cacheKey := c.getClusterResourceInfoCacheKey(clusterName)
	if cache, exists := c.caches[cacheKey]; exists && !cache.IsExpired() {
		return cache.GetData().(apis.ClusterResourceInfo), nil
	}
	clusterInfo, err := multicluster.GetClusterInfo(ctx, c.k8sClient, clusterName)
	if err != nil {
		return apis.ClusterResourceInfo{}, err
	}
	var storageClassList []string
	for _, cls := range clusterInfo.StorageClasses.Items {
		storageClassList = append(storageClassList, cls.Name)
	}
	getUsed := func(cap resource.Quantity, alloc resource.Quantity) *resource.Quantity {
		used := cap.DeepCopy()
		used.Sub(alloc)
		return &used
	}
	// TODO add support for gpu capacity
	clusterResourceInfo := apis.ClusterResourceInfo{
		WorkerNumber:     clusterInfo.WorkerNumber,
		MasterNumber:     clusterInfo.MasterNumber,
		MemoryCapacity:   clusterInfo.MemoryCapacity.Value(),
		CPUCapacity:      clusterInfo.CPUCapacity.Value(),
		GPUCapacity:      0,
		PodCapacity:      clusterInfo.PodCapacity.Value(),
		MemoryUsed:       getUsed(clusterInfo.MemoryCapacity, clusterInfo.MemoryAllocatable).Value(),
		CPUUsed:          getUsed(clusterInfo.CPUCapacity, clusterInfo.CPUAllocatable).Value(),
		GPUUsed:          0,
		PodUsed:          getUsed(clusterInfo.PodCapacity, clusterInfo.PodAllocatable).Value(),
		StorageClassList: storageClassList,
	}
	c.caches[cacheKey] = utils2.NewMemoryCache(clusterResourceInfo, time.Minute)
	return clusterResourceInfo, nil
}

func (c *clusterUsecaseImpl) ListCloudClusters(ctx context.Context, provider string, req apis.AccessKeyRequest, pageNumber int, pageSize int) (*apis.ListCloudClusterResponse, error) {
	p, err := cloudprovider.GetClusterProvider(provider, req.AccessKeyID, req.AccessKeySecret, c.k8sClient)
	if err != nil {
		log.Logger.Errorf("failed to get cluster provider: %s", err.Error())
		return nil, bcode.ErrInvalidCloudClusterProvider
	}
	clusters, total, err := p.ListCloudClusters(pageNumber, pageSize)
	if err != nil {
		if p.IsInvalidKey(err) {
			return nil, bcode.ErrInvalidAccessKeyOrSecretKey
		}
		log.Logger.Errorf("failed to list cloud clusters: %s", err.Error())
		return nil, bcode.ErrGetCloudClusterFailure
	}
	resp := &apis.ListCloudClusterResponse{
		Clusters: []cloudprovider.CloudCluster{},
		Total:    total,
	}
	for _, cluster := range clusters {
		resp.Clusters = append(resp.Clusters, *cluster)
	}
	return resp, nil
}

func (c *clusterUsecaseImpl) ConnectCloudCluster(ctx context.Context, provider string, req apis.ConnectCloudClusterRequest) (*apis.ClusterBase, error) {
	p, err := cloudprovider.GetClusterProvider(provider, req.AccessKeyID, req.AccessKeySecret, c.k8sClient)
	if err != nil {
		log.Logger.Errorf("failed to get cluster provider: %s", err.Error())
		return nil, bcode.ErrInvalidCloudClusterProvider
	}
	kubeConfig, err := p.GetClusterKubeConfig(req.ClusterID)
	if err != nil {
		log.Logger.Errorf("failed to get cluster kubeConfig: %s", err.Error())
		return nil, bcode.ErrGetCloudClusterFailure
	}
	cluster, err := p.GetClusterInfo(req.ClusterID)
	if err != nil {
		if p.IsInvalidKey(err) {
			return nil, bcode.ErrInvalidAccessKeyOrSecretKey
		}
		log.Logger.Errorf("failed to get cluster info: %s", err.Error())
		return nil, bcode.ErrGetCloudClusterFailure
	}
	createReq := apis.CreateClusterRequest{
		Name:        req.Name,
		Alias:       req.Alias,
		Description: req.Description,
		Icon:        req.Icon,
		Labels:      req.Labels,
		KubeConfig:  kubeConfig,
	}
	return c.createKubeCluster(ctx, createReq, cluster)
}

func (c *clusterUsecaseImpl) CreateCloudCluster(ctx context.Context, provider string, req apis.CreateCloudClusterRequest) (*apis.CreateCloudClusterResponse, error) {
	p, err := cloudprovider.GetClusterProvider(provider, req.AccessKeyID, req.AccessKeySecret, c.k8sClient)
	if err != nil {
		log.Logger.Errorf("failed to get cluster provider: %s", err.Error())
		return nil, bcode.ErrInvalidCloudClusterProvider
	}
	_, err = p.CreateCloudCluster(ctx, req.Name, req.Zone, req.WorkerNumber, req.CPUCoresPerWorker, req.MemoryPerWorker)
	if err != nil {
		if kerrors.IsAlreadyExists(err) {
			return nil, bcode.ErrCloudClusterAlreadyExists
		}
		log.Logger.Errorf("failed to bootstrap terraform configuration: %s", err.Error())
		return nil, bcode.ErrBootstrapTerraformConfiguration
	}
	return c.GetCloudClusterCreationStatus(ctx, provider, req.Name)
}

func (c *clusterUsecaseImpl) convertTerraformConfigurationStateIntoCloudClusterCreationStatus(cfg v1beta1.Configuration) (status string, clusterID string, err error) {
	status = string(cfg.Status.Apply.State)
	if status == "" {
		return "Initializing", "", nil
	}
	if cfg.DeletionTimestamp != nil {
		return "Deleting", "", nil
	}
	if status == string(types.Available) {
		cid, ok := cfg.Status.Apply.Outputs["CLUSTER_ID"]
		if !ok {
			status = "ClusterIDNotFound"
			return status, "", bcode.ErrClusterIDNotFoundInTerraformConfiguration
		}
		return status, cid.Value, nil
	}
	return status, "", nil
}

func (c *clusterUsecaseImpl) getCloudClusterCreationStatus(ctx context.Context, provider string, cloudClusterName string) (*apis.CreateCloudClusterResponse, *v1beta1.Configuration, error) {
	terraformConfigurationName := cloudprovider.GetCloudClusterFullName(provider, cloudClusterName)
	cfg := &v1beta1.Configuration{
		ObjectMeta: v1.ObjectMeta{
			Name:      terraformConfigurationName,
			Namespace: util.GetRuntimeNamespace(),
		},
	}
	if err := c.k8sClient.Get(ctx, client.ObjectKeyFromObject(cfg), cfg); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil, bcode.ErrTerraformConfigurationNotFound
		}
		return nil, nil, err
	}
	status, clusterID, err := c.convertTerraformConfigurationStateIntoCloudClusterCreationStatus(*cfg)
	if err != nil {
		return nil, cfg, err
	}
	return &apis.CreateCloudClusterResponse{Name: cloudClusterName, Status: status, ClusterID: clusterID}, cfg, nil
}

func (c *clusterUsecaseImpl) GetCloudClusterCreationStatus(ctx context.Context, provider string, cloudClusterName string) (*apis.CreateCloudClusterResponse, error) {
	resp, _, err := c.getCloudClusterCreationStatus(ctx, provider, cloudClusterName)
	return resp, err
}

func (c *clusterUsecaseImpl) ListCloudClusterCreation(ctx context.Context, provider string) (*apis.ListCloudClusterCreationResponse, error) {
	cfgs := v1beta1.ConfigurationList{}
	if err := c.k8sClient.List(ctx, &cfgs, client.HasLabels{cloudprovider.CloudClusterCreatorLabelKey}, client.InNamespace(util.GetRuntimeNamespace())); err != nil {
		return nil, err
	}
	var creations []apis.CreateCloudClusterResponse
	for _, cfg := range cfgs.Items {
		prefix := "cloud-cluster-" + provider + "-"
		if strings.HasPrefix(cfg.Name, prefix) {
			status, clusterID, _ := c.convertTerraformConfigurationStateIntoCloudClusterCreationStatus(cfg)
			name := strings.TrimPrefix(cfg.Name, prefix)
			creations = append(creations, apis.CreateCloudClusterResponse{Name: name, Status: status, ClusterID: clusterID})
		}
	}
	return &apis.ListCloudClusterCreationResponse{Creations: creations}, nil
}

func (c *clusterUsecaseImpl) DeleteCloudClusterCreation(ctx context.Context, provider string, cloudClusterName string) (*apis.CreateCloudClusterResponse, error) {
	resp, cfg, err := c.getCloudClusterCreationStatus(ctx, provider, cloudClusterName)
	if err != nil {
		return resp, err
	}
	if err = c.k8sClient.Delete(ctx, cfg); err != nil {
		if kerrors.IsNotFound(err) {
			return resp, nil
		}
		return nil, err
	}
	return resp, err
}

func newClusterBaseFromCluster(cluster *model.Cluster) *apis.ClusterBase {
	return &apis.ClusterBase{
		Name:        cluster.Name,
		Alias:       cluster.Alias,
		Description: cluster.Description,
		Icon:        cluster.Icon,
		Labels:      cluster.Labels,

		APIServerURL: cluster.APIServerURL,
		DashboardURL: cluster.DashboardURL,
		Provider:     cluster.Provider,

		Status: cluster.Status,
		Reason: cluster.Reason,
	}
}

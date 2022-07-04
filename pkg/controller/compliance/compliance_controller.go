// Copyright (c) 2020-2022 Tigera, Inc. All rights reserved.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package compliance

import (
	"context"
	"fmt"
	"time"

	v3 "github.com/tigera/api/pkg/apis/projectcalico/v3"
	"github.com/tigera/operator/pkg/render/common/networkpolicy"
	"k8s.io/apimachinery/pkg/types"

	"github.com/go-logr/logr"
	operatorv1 "github.com/tigera/operator/api/v1"
	"github.com/tigera/operator/pkg/common"
	"github.com/tigera/operator/pkg/controller/certificatemanager"
	"github.com/tigera/operator/pkg/controller/options"
	"github.com/tigera/operator/pkg/controller/status"
	"github.com/tigera/operator/pkg/controller/utils"
	"github.com/tigera/operator/pkg/controller/utils/imageset"
	"github.com/tigera/operator/pkg/dns"
	"github.com/tigera/operator/pkg/render"
	rcertificatemanagement "github.com/tigera/operator/pkg/render/certificatemanagement"
	relasticsearch "github.com/tigera/operator/pkg/render/common/elasticsearch"
	"github.com/tigera/operator/pkg/tls/certificatemanagement"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const ResourceName = "compliance"

var log = logf.Log.WithName("controller_compliance")

// Add creates a new Compliance Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, opts options.AddOptions) error {
	if !opts.EnterpriseCRDExists {
		// No need to start this controller.
		return nil
	}
	licenseAPIReady := &utils.ReadyFlag{}
	policyWatchesReady := &utils.ReadyFlag{}

	// create the reconciler
	reconciler := newReconciler(mgr, opts, licenseAPIReady, policyWatchesReady)

	// Create a new controller
	controller, err := controller.New("compliance-controller", mgr, controller.Options{Reconciler: reconcile.Reconciler(reconciler)})
	if err != nil {
		return err
	}

	k8sClient, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		log.Error(err, "Failed to establish a connection to k8s")
		return err
	}

	go utils.WaitToAddLicenseKeyWatch(controller, k8sClient, log, licenseAPIReady)

	go utils.WaitToAddNetworkPolicyWatches(controller, k8sClient, log, policyWatchesReady, []types.NamespacedName{
		{Name: render.ComplianceAccessPolicyName, Namespace: render.ComplianceNamespace},
		{Name: render.ComplianceServerPolicyName, Namespace: render.ComplianceNamespace},
		{Name: networkpolicy.TigeraComponentDefaultDenyPolicyName, Namespace: render.ComplianceNamespace},
	})

	return add(mgr, controller)
}

// newReconciler returns a new *reconcile.Reconciler
func newReconciler(mgr manager.Manager, opts options.AddOptions, licenseAPIReady *utils.ReadyFlag, policyWatchesReady *utils.ReadyFlag) reconcile.Reconciler {
	r := &ReconcileCompliance{
		client:             mgr.GetClient(),
		scheme:             mgr.GetScheme(),
		provider:           opts.DetectedProvider,
		status:             status.New(mgr.GetClient(), "compliance", opts.KubernetesVersion),
		clusterDomain:      opts.ClusterDomain,
		licenseAPIReady:    licenseAPIReady,
		policyWatchesReady: policyWatchesReady,
		usePSP:             opts.UsePSP,
	}
	r.status.Run(opts.ShutdownContext)
	return r
}

// add adds watches for resources that are available at startup
func add(mgr manager.Manager, c controller.Controller) error {
	var err error

	// Watch for changes to primary resource Compliance
	err = c.Watch(&source.Kind{Type: &operatorv1.Compliance{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	if err = utils.AddNetworkWatch(c); err != nil {
		return fmt.Errorf("compliance-controller failed to watch Network resource: %w", err)
	}

	if err = imageset.AddImageSetWatch(c); err != nil {
		return fmt.Errorf("compliance-controller failed to watch ImageSet: %w", err)
	}

	if err = utils.AddAPIServerWatch(c); err != nil {
		return fmt.Errorf("compliance-controller failed to watch APIServer resource: %w", err)
	}

	// Watch the given secrets in each both the compliance and operator namespaces
	for _, namespace := range []string{common.OperatorNamespace(), render.ComplianceNamespace} {
		for _, secretName := range []string{
			render.TigeraElasticsearchGatewaySecret, render.ElasticsearchComplianceBenchmarkerUserSecret,
			render.ElasticsearchComplianceControllerUserSecret, render.ElasticsearchComplianceReporterUserSecret,
			render.ElasticsearchComplianceSnapshotterUserSecret, render.ElasticsearchComplianceServerUserSecret,
			render.ComplianceServerCertSecret, render.ManagerInternalTLSSecretName, certificatemanagement.CASecretName,
		} {
			if err = utils.AddSecretsWatch(c, secretName, namespace); err != nil {
				return fmt.Errorf("compliance-controller failed to watch the secret '%s' in '%s' namespace: %w", secretName, namespace, err)
			}
		}
	}

	if err = utils.AddConfigMapWatch(c, relasticsearch.ClusterConfigConfigMapName, common.OperatorNamespace()); err != nil {
		return fmt.Errorf("compliance-controller failed to watch the ConfigMap resource: %w", err)
	}

	// Watch for changes to primary resource ManagementCluster
	err = c.Watch(&source.Kind{Type: &operatorv1.ManagementCluster{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return fmt.Errorf("compliance-controller failed to watch primary resource: %w", err)
	}

	// Watch for changes to primary resource ManagementClusterConnection
	err = c.Watch(&source.Kind{Type: &operatorv1.ManagementClusterConnection{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return fmt.Errorf("compliance-controller failed to watch primary resource: %w", err)
	}

	err = c.Watch(&source.Kind{Type: &operatorv1.Authentication{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return fmt.Errorf("compliance-controller failed to watch resource: %w", err)
	}

	// Watch for changes to TigeraStatus.
	if err = utils.AddTigeraStatusWatch(c, ResourceName); err != nil {
		return fmt.Errorf("compliance-controller failed to watch compliance Tigerastatus: %w", err)
	}

	return nil
}

// blank assignment to verify that ReconcileCompliance implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileCompliance{}

// ReconcileCompliance reconciles a Compliance object
type ReconcileCompliance struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client             client.Client
	scheme             *runtime.Scheme
	provider           operatorv1.Provider
	status             status.StatusManager
	clusterDomain      string
	licenseAPIReady    *utils.ReadyFlag
	policyWatchesReady *utils.ReadyFlag
	usePSP             bool
}

func GetCompliance(ctx context.Context, cli client.Client) (*operatorv1.Compliance, error) {
	instance := &operatorv1.Compliance{}
	err := cli.Get(ctx, utils.DefaultTSEEInstanceKey, instance)
	if err != nil {
		return nil, err
	}
	return instance, nil
}

// Reconcile reads that state of the cluster for a Compliance object and makes changes based on the state read
// and what is in the Compliance.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileCompliance) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling Compliance")

	// Fetch the Compliance instance
	instance, err := GetCompliance(ctx, r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			reqLogger.Info("Compliance config not found")
			r.status.OnCRNotFound()
			return reconcile.Result{}, nil
		}
		r.SetDegraded(operatorv1.ResourceReadError, "Error querying compliance", err, reqLogger)
		return reconcile.Result{}, err
	}
	r.status.OnCRFound()
	reqLogger.V(2).Info("Loaded config", "config", instance)

	//Set the meta info in the tigerastatus like observedGenerations
	if instance != nil {
		defer r.status.SetMetaData(&instance.ObjectMeta)
	}

	// Changes for updating compliance status conditions
	if request.Name == ResourceName && request.Namespace == "" {
		ts := &operatorv1.TigeraStatus{}
		err := r.client.Get(ctx, types.NamespacedName{Name: ResourceName}, ts)
		if err != nil {
			return reconcile.Result{}, err
		}
		instance.Status.Conditions = status.UpdateStatusCondition(instance.Status.Conditions, ts.Status.Conditions)
		if err := r.client.Status().Update(ctx, instance); err != nil {
			log.WithValues("reason", err).Info("Failed to create compliance status conditions.")
			return reconcile.Result{}, err
		}
	}

	if !utils.IsAPIServerReady(r.client, reqLogger) {
		r.status.SetDegraded(string(operatorv1.ResourceNotReady), "Waiting for Tigera API server to be ready")
		return reconcile.Result{}, err
	}

	if !r.policyWatchesReady.IsReady() {
		r.status.SetDegraded("Waiting for NetworkPolicy watches to be established", "")
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Ensure the allow-tigera tier exists, before rendering any network policies within it.
	if err := r.client.Get(ctx, client.ObjectKey{Name: networkpolicy.TigeraComponentTierName}, &v3.Tier{}); err != nil {
		if errors.IsNotFound(err) {
			r.status.SetDegraded("Waiting for allow-tigera tier to be created", err.Error())
			return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
		} else {
			log.Error(err, "Error querying allow-tigera tier")
			r.status.SetDegraded("Error querying allow-tigera tier", err.Error())
			return reconcile.Result{}, err
		}
	}

	if !r.licenseAPIReady.IsReady() {
		r.status.SetDegraded(string(operatorv1.ResourceNotReady), "Waiting for LicenseKeyAPI to be ready")
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}

	license, err := utils.FetchLicenseKey(ctx, r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			r.SetDegraded(operatorv1.ResourceNotFound, "License not found", err, reqLogger)
			return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
		}
		r.SetDegraded(operatorv1.ResourceReadError, "Error querying license", err, reqLogger)
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Query for the installation object.
	variant, network, err := utils.GetInstallation(ctx, r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			r.SetDegraded(operatorv1.ResourceNotFound, "Installation not found", err, reqLogger)
			return reconcile.Result{}, err
		}
		r.SetDegraded(operatorv1.ResourceReadError, "Error querying installation", err, reqLogger)
		return reconcile.Result{}, err
	}

	pullSecrets, err := utils.GetNetworkingPullSecrets(network, r.client)
	if err != nil {
		r.SetDegraded(operatorv1.ResourceReadError, "Failed to retrieve pull secrets", err, reqLogger)
		return reconcile.Result{}, err
	}

	esClusterConfig, err := utils.GetElasticsearchClusterConfig(ctx, r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			r.SetDegraded(operatorv1.ResourceNotReady, "Elasticsearch cluster configuration is not available, waiting for it to become available", err, reqLogger)
			return reconcile.Result{}, nil
		}
		r.SetDegraded(operatorv1.ResourceReadError, "Failed to get the elasticsearch cluster configuration", err, reqLogger)
		return reconcile.Result{}, err
	}

	secretsToWatch := []string{
		render.ElasticsearchComplianceBenchmarkerUserSecret, render.ElasticsearchComplianceControllerUserSecret,
		render.ElasticsearchComplianceReporterUserSecret, render.ElasticsearchComplianceSnapshotterUserSecret,
	}

	managementCluster, err := utils.GetManagementCluster(ctx, r.client)
	if err != nil {
		r.SetDegraded(operatorv1.ResourceReadError, "Error reading ManagementCluster", err, reqLogger)
		return reconcile.Result{}, err
	}

	managementClusterConnection, err := utils.GetManagementClusterConnection(ctx, r.client)
	if err != nil {
		r.SetDegraded(operatorv1.ResourceReadError, "Error reading ManagementClusterConnection", err, reqLogger)
		return reconcile.Result{}, err
	}

	if managementClusterConnection != nil && managementCluster != nil {
		err = fmt.Errorf("having both a ManagementCluster and a ManagementClusterConnection is not supported")
		r.SetDegraded(operatorv1.ResourceValidationError, "", err, reqLogger)
		return reconcile.Result{}, err
	}

	// Compliance server is only for Standalone or Management clusters
	if managementClusterConnection == nil {
		secretsToWatch = append(secretsToWatch, render.ElasticsearchComplianceServerUserSecret)
	}

	esSecrets, err := utils.ElasticsearchSecrets(ctx, secretsToWatch, r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			r.SetDegraded(operatorv1.ResourceNotReady, "Elasticsearch secrets are not available yet, waiting until they become available", err, reqLogger)
			return reconcile.Result{}, nil
		}
		r.SetDegraded(operatorv1.ResourceReadError, "Failed to get Elasticsearch credentials", err, reqLogger)
		return reconcile.Result{}, err
	}

	certificateManager, err := certificatemanager.Create(r.client, network, r.clusterDomain)
	if err != nil {
		r.SetDegraded(operatorv1.ResourceCreateError, "Unable to create the Tigera CA", err, reqLogger)
		return reconcile.Result{}, err
	}
	var managerInternalTLSSecret certificatemanagement.CertificateInterface
	if managementCluster != nil {
		managerInternalTLSSecret, err = certificateManager.GetCertificate(r.client, render.ManagerInternalTLSSecretName, common.OperatorNamespace())
		if err != nil {
			r.SetDegraded(operatorv1.ResourceValidationError, fmt.Sprintf("failed to retrieve / validate  %s", render.ManagerInternalTLSSecretName), err, reqLogger)
			return reconcile.Result{}, err
		}
	}
	esgwCertificate, err := certificateManager.GetCertificate(r.client, relasticsearch.PublicCertSecret, common.OperatorNamespace())
	if err != nil {
		r.SetDegraded(operatorv1.ResourceValidationError, fmt.Sprintf("Failed to retrieve / validate  %s", relasticsearch.PublicCertSecret), err, reqLogger)
		return reconcile.Result{}, err
	} else if esgwCertificate == nil {
		log.Info("Elasticsearch gateway certificate is not available yet, waiting until they become available")
		r.status.SetDegraded(string(operatorv1.ResourceNotReady), "Elasticsearch gateway certificate are not available yet, waiting until they become available")
		return reconcile.Result{}, nil
	}
	trustedBundle := certificateManager.CreateTrustedBundle(managerInternalTLSSecret, esgwCertificate)

	var complianceServerCertSecret certificatemanagement.KeyPairInterface
	if managementClusterConnection == nil {
		complianceServerCertSecret, err = certificateManager.GetOrCreateKeyPair(
			r.client,
			render.ComplianceServerCertSecret,
			common.OperatorNamespace(),
			dns.GetServiceDNSNames(render.ComplianceServiceName, render.ComplianceNamespace, r.clusterDomain))
		if err != nil {
			r.SetDegraded(operatorv1.ResourceValidationError, fmt.Sprintf("failed to retrieve / validate  %s", render.ComplianceServerCertSecret), err, reqLogger)
			return reconcile.Result{}, err
		}
	}
	certificateManager.AddToStatusManager(r.status, render.ComplianceNamespace)

	// Fetch the Authentication spec. If present, we use to configure user authentication.
	authenticationCR, err := utils.GetAuthentication(ctx, r.client)
	if err != nil && !errors.IsNotFound(err) {
		r.SetDegraded(operatorv1.ResourceReadError, "Error querying Authentication", err, reqLogger)
		return reconcile.Result{}, err
	}
	if authenticationCR != nil && authenticationCR.Status.State != operatorv1.TigeraStatusReady {
		r.status.SetDegraded(string(operatorv1.ResourceNotReady), fmt.Sprintf("Authentication is not ready - authenticationCR status: %s", authenticationCR.Status.State))
		return reconcile.Result{}, nil
	}

	// Create a component handler to manage the rendered component.
	handler := utils.NewComponentHandler(log, r.client, r.scheme, instance)

	keyValidatorConfig, err := utils.GetKeyValidatorConfig(ctx, r.client, authenticationCR, r.clusterDomain)
	if err != nil {
		r.SetDegraded(operatorv1.ResourceValidationError, "Failed to process the authentication CR.", err, reqLogger)
		return reconcile.Result{}, err
	}

	reqLogger.V(3).Info("rendering components")
	hasNoLicense := !utils.IsFeatureActive(license, common.ComplianceFeature)
	openshift := r.provider == operatorv1.ProviderOpenShift
	complianceCfg := &render.ComplianceConfiguration{
		ESSecrets:                   esSecrets,
		TrustedBundle:               trustedBundle,
		Installation:                network,
		ComplianceServerCertSecret:  complianceServerCertSecret,
		ESClusterConfig:             esClusterConfig,
		PullSecrets:                 pullSecrets,
		Openshift:                   openshift,
		ManagementCluster:           managementCluster,
		ManagementClusterConnection: managementClusterConnection,
		KeyValidatorConfig:          keyValidatorConfig,
		ClusterDomain:               r.clusterDomain,
		HasNoLicense:                hasNoLicense,
		UsePSP:                      r.usePSP,
	}
	// Render the desired objects from the CRD and create or update them.
	comp, err := render.Compliance(complianceCfg)
	if err != nil {
		r.SetDegraded(operatorv1.ResourceRenderingError, "Error rendering Compliance", err, reqLogger)
		return reconcile.Result{}, err
	}

	if err = imageset.ApplyImageSet(ctx, r.client, variant, comp); err != nil {
		r.SetDegraded(operatorv1.ResourceUpdateError, "Error with images from ImageSet", err, reqLogger)
		return reconcile.Result{}, err
	}
	certificateComponent := rcertificatemanagement.CertificateManagement(&rcertificatemanagement.Config{
		Namespace:       render.ComplianceNamespace,
		ServiceAccounts: []string{render.ComplianceServerSAName},
		KeyPairOptions: []rcertificatemanagement.KeyPairOption{
			rcertificatemanagement.NewKeyPairOption(complianceServerCertSecret, true, true),
		},
		TrustedBundle: trustedBundle,
	})

	for _, comp := range []render.Component{comp, certificateComponent} {
		if err := handler.CreateOrUpdateOrDelete(ctx, comp, r.status); err != nil {
			r.SetDegraded(operatorv1.ResourceUpdateError, "Error creating / updating / deleting resource", err, reqLogger)
			return reconcile.Result{}, err
		}
	}

	if hasNoLicense {
		log.V(4).Info("Compliance is not activated as part of this license")
		r.status.SetDegraded(string(operatorv1.ResourceValidationError), "Feature is not active - License does not support this feature")
		return reconcile.Result{}, nil
	}

	// Clear the degraded bit if we've reached this far.
	r.status.ClearDegraded()

	if !r.status.IsAvailable() {
		// Schedule a kick to check again in the near future. Hopefully by then
		// things will be available.
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Everything is available - update the CRD status.
	instance.Status.State = operatorv1.TigeraStatusReady
	if err = r.client.Status().Update(ctx, instance); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileCompliance) SetDegraded(reason operatorv1.TigeraStatusReason, message string, err error, log logr.Logger) {
	log.WithValues(string(reason), message).Error(err, string(reason))
	errormsg := ""
	if err != nil {
		errormsg = err.Error()
	}
	r.status.SetDegraded(string(reason), fmt.Sprintf("%s - Error: %s", message, errormsg))
}

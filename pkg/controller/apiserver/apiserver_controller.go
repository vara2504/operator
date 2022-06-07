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

package apiserver

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	operatorv1 "github.com/tigera/operator/api/v1"
	"github.com/tigera/operator/pkg/common"
	"github.com/tigera/operator/pkg/controller/certificatemanager"
	"github.com/tigera/operator/pkg/controller/k8sapi"
	"github.com/tigera/operator/pkg/controller/options"
	"github.com/tigera/operator/pkg/controller/status"
	"github.com/tigera/operator/pkg/controller/utils"
	"github.com/tigera/operator/pkg/controller/utils/imageset"
	"github.com/tigera/operator/pkg/dns"
	"github.com/tigera/operator/pkg/render"
	rcertificatemanagement "github.com/tigera/operator/pkg/render/certificatemanagement"
	rmeta "github.com/tigera/operator/pkg/render/common/meta"
	"github.com/tigera/operator/pkg/tls/certificatemanagement"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const ResourceName string = "apiserver"

var log = logf.Log.WithName("controller_apiserver")

// Add creates a new APIServer Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, opts options.AddOptions) error {
	return add(mgr, newReconciler(mgr, opts))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, opts options.AddOptions) *ReconcileAPIServer {
	r := &ReconcileAPIServer{
		client:              mgr.GetClient(),
		scheme:              mgr.GetScheme(),
		provider:            opts.DetectedProvider,
		amazonCRDExists:     opts.AmazonCRDExists,
		enterpriseCRDsExist: opts.EnterpriseCRDExists,
		status:              status.New(mgr.GetClient(), "apiserver", opts.KubernetesVersion),
		clusterDomain:       opts.ClusterDomain,
	}
	r.status.Run(opts.ShutdownContext)
	return r
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r *ReconcileAPIServer) error {
	// Create a new controller
	c, err := controller.New("apiserver-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return fmt.Errorf("Failed to create apiserver-controller: %v", err)
	}

	// Watch for changes to primary resource APIServer
	err = c.Watch(&source.Kind{Type: &operatorv1.APIServer{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		log.V(5).Info("Failed to create APIServer watch", "err", err)
		return fmt.Errorf("apiserver-controller failed to watch primary resource: %v", err)
	}

	if err = utils.AddNetworkWatch(c); err != nil {
		log.V(5).Info("Failed to create network watch", "err", err)
		return fmt.Errorf("apiserver-controller failed to watch Tigera network resource: %v", err)
	}

	if err = utils.AddConfigMapWatch(c, render.K8sSvcEndpointConfigMapName, common.OperatorNamespace()); err != nil {
		return fmt.Errorf("apiserver-controller failed to watch ConfigMap %s: %w", render.K8sSvcEndpointConfigMapName, err)
	}

	if r.amazonCRDExists {
		err = c.Watch(&source.Kind{Type: &operatorv1.AmazonCloudIntegration{}}, &handler.EnqueueRequestForObject{})
		if err != nil {
			log.V(5).Info("Failed to create AmazonCloudIntegration watch", "err", err)
			return fmt.Errorf("apiserver-controller failed to watch primary resource: %v", err)
		}
	}

	if r.enterpriseCRDsExist {
		// Watch for changes to primary resource ManagementCluster
		err = c.Watch(&source.Kind{Type: &operatorv1.ManagementCluster{}}, &handler.EnqueueRequestForObject{})
		if err != nil {
			return fmt.Errorf("apiserver-controller failed to watch primary resource: %v", err)
		}

		// Watch for changes to primary resource ManagementClusterConnection
		err = c.Watch(&source.Kind{Type: &operatorv1.ManagementClusterConnection{}}, &handler.EnqueueRequestForObject{})
		if err != nil {
			return fmt.Errorf("apiserver-controller failed to watch primary resource: %v", err)
		}

		for _, namespace := range []string{common.OperatorNamespace(), rmeta.APIServerNamespace(operatorv1.TigeraSecureEnterprise)} {
			if err = utils.AddSecretsWatch(c, render.VoltronTunnelSecretName, namespace); err != nil {
				return fmt.Errorf("apiserver-controller failed to watch the Secret resource: %v", err)
			}
		}

		// Watch for changes to authentication
		err = c.Watch(&source.Kind{Type: &operatorv1.Authentication{}}, &handler.EnqueueRequestForObject{})
		if err != nil {
			return fmt.Errorf("apiserver-controller failed to watch resource: %w", err)
		}
	}

	for _, secretName := range []string{"calico-apiserver-certs", "tigera-apiserver-certs", render.PacketCaptureCertSecret,
		certificatemanagement.CASecretName, render.DexTLSSecretName} {
		if err = utils.AddSecretsWatch(c, secretName, common.OperatorNamespace()); err != nil {
			return fmt.Errorf("apiserver-controller failed to watch the Secret resource: %v", err)
		}
	}

	if err = imageset.AddImageSetWatch(c); err != nil {
		return fmt.Errorf("apiserver-controller failed to watch ImageSet: %w", err)
	}

	// Watch for changes to TigeraStatus.
	err = c.Watch(&source.Kind{Type: &operatorv1.TigeraStatus{ObjectMeta: metav1.ObjectMeta{Name: ResourceName}}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return fmt.Errorf("apiserver-controller failed to watch apiserver Tigerastatus: %w", err)
	}

	log.V(5).Info("Controller created and Watches setup")
	return nil
}

// blank assignment to verify that ReconcileAPIServer implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileAPIServer{}

// ReconcileAPIServer reconciles a APIServer object
type ReconcileAPIServer struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client              client.Client
	scheme              *runtime.Scheme
	provider            operatorv1.Provider
	amazonCRDExists     bool
	enterpriseCRDsExist bool
	status              status.StatusManager
	clusterDomain       string
}

// Reconcile reads that state of the cluster for a APIServer object and makes changes based on the state read
// and what is in the APIServer.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileAPIServer) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling APIServer")

	instance, msg, err := utils.GetAPIServer(ctx, r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Info("APIServer config not found")
			r.status.OnCRNotFound()
			return reconcile.Result{}, nil
		}
		r.SetDegraded(operatorv1.ResourceReadError, fmt.Sprintf("An error occurred when querying the APIServer resource: %s", msg), err, reqLogger)
		return reconcile.Result{}, err
	}
	r.status.OnCRFound()
	reqLogger.V(2).Info("Loaded config", "config", instance)

	// Changes for updating apiserver status conditions
	if request.Name == ResourceName && request.Namespace == "" {
		ts := &operatorv1.TigeraStatus{}
		err := r.client.Get(ctx, types.NamespacedName{Name: ResourceName}, ts)
		if err != nil {
			return reconcile.Result{}, err
		}
		instance.Status.Conditions = status.UpdateStatusCondition(instance.Status.Conditions, ts.Status.Conditions, instance.GetGeneration())
		if err := r.client.Status().Update(ctx, instance); err != nil {
			log.WithValues("reason", err).Info("Failed to create apiserver status conditions.")
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	// Query for the installation object.
	variant, network, err := utils.GetInstallation(context.Background(), r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			r.SetDegraded(operatorv1.ResourceNotFound, "Installation not found", err, reqLogger)
			return reconcile.Result{}, err
		}
		r.SetDegraded(operatorv1.ResourceReadError, "Error querying installation", err, reqLogger)
		return reconcile.Result{}, err
	}
	if variant == "" {
		r.status.SetDegraded(string(operatorv1.ResourceNotReady), "Waiting for Installation to be ready")
		return reconcile.Result{}, nil
	}
	ns := rmeta.APIServerNamespace(variant)

	certificateManager, err := certificatemanager.Create(r.client, network, r.clusterDomain)
	if err != nil {
		r.SetDegraded(operatorv1.ResourceCreateError, "Unable to create the Tigera CA", err, reqLogger)
		return reconcile.Result{}, err
	}

	// We need separate certificates for OSS vs Enterprise.
	secretName := render.ProjectCalicoApiServerTLSSecretName(network.Variant)
	tlsSecret, err := certificateManager.GetOrCreateKeyPair(r.client, secretName, common.OperatorNamespace(), dns.GetServiceDNSNames(render.ProjectCalicoApiServerServiceName(network.Variant), rmeta.APIServerNamespace(network.Variant), r.clusterDomain))
	if err != nil {
		r.SetDegraded(operatorv1.ResourceCreateError, "Unable to get or create tls key pair", err, reqLogger)
		return reconcile.Result{}, err
	}

	certificateManager.AddToStatusManager(r.status, ns)

	pullSecrets, err := utils.GetNetworkingPullSecrets(network, r.client)
	if err != nil {
		r.SetDegraded(operatorv1.ResourceReadError, "Error retrieving pull secrets", err, reqLogger)
		return reconcile.Result{}, err
	}

	// Query enterprise-only data.
	var tunnelCASecret certificatemanagement.KeyPairInterface
	var amazon *operatorv1.AmazonCloudIntegration
	var managementCluster *operatorv1.ManagementCluster
	var managementClusterConnection *operatorv1.ManagementClusterConnection
	var tunnelSecretPassthrough render.Component
	if variant == operatorv1.TigeraSecureEnterprise {
		managementCluster, err = utils.GetManagementCluster(ctx, r.client)
		if err != nil {
			r.SetDegraded(operatorv1.ResourceReadError, "Error reading ManagementCluster", err, reqLogger)
			return reconcile.Result{}, err
		}

		managementClusterConnection, err = utils.GetManagementClusterConnection(ctx, r.client)
		if err != nil {
			r.SetDegraded(operatorv1.ResourceReadError, "Error reading ManagementClusterConnection", err, reqLogger)
			return reconcile.Result{}, err
		}

		if managementClusterConnection != nil && managementCluster != nil {
			err = fmt.Errorf("having both a ManagementCluster and a ManagementClusterConnection is not supported")
			r.SetDegraded(operatorv1.ResourceValidationError, "", err, reqLogger)
			return reconcile.Result{}, err
		}

		if managementCluster != nil {
			tunnelCASecret, err = certificateManager.GetKeyPair(r.client, render.VoltronTunnelSecretName, common.OperatorNamespace())
			if tunnelCASecret == nil {
				// tunnelCASecret is a secret unaffected by the last two args (dnsNames and clusterDomain).
				tunnelCASecret, err = certificatemanagement.NewKeyPair(render.VoltronTunnelSecret(), nil, "")

				// Creating the voltron tunnel secret is not (yet) supported by certificate mananger.
				tunnelSecretPassthrough = render.NewPassthrough(tunnelCASecret.Secret(common.OperatorNamespace()))
			}
			if err != nil {
				r.SetDegraded(operatorv1.ResourceCreateError, "Unable to get or create the tunnel secret", err, reqLogger)
				return reconcile.Result{}, err
			}
		}

		if r.amazonCRDExists {
			amazon, err = utils.GetAmazonCloudIntegration(ctx, r.client)
			if errors.IsNotFound(err) {
				amazon = nil
			} else if err != nil {
				r.SetDegraded(operatorv1.ResourceReadError, "Error reading AmazonCloudIntegration", err, reqLogger)
				return reconcile.Result{}, err
			}
		}
	}

	err = utils.GetK8sServiceEndPoint(r.client)
	if err != nil {
		r.SetDegraded(operatorv1.ResourceReadError, "Error reading services endpoint configmap", err, reqLogger)
		return reconcile.Result{}, err
	}
	// Create a component handler to manage the rendered component.
	handler := utils.NewComponentHandler(log, r.client, r.scheme, instance)

	// Render the desired objects from the CRD and create or update them.
	reqLogger.V(3).Info("rendering components")

	apiServerCfg := render.APIServerConfiguration{
		K8SServiceEndpoint:          k8sapi.Endpoint,
		Installation:                network,
		ForceHostNetwork:            false,
		ManagementCluster:           managementCluster,
		ManagementClusterConnection: managementClusterConnection,
		AmazonCloudIntegration:      amazon,
		TLSKeyPair:                  tlsSecret,
		PullSecrets:                 pullSecrets,
		Openshift:                   r.provider == operatorv1.ProviderOpenShift,
		TunnelCASecret:              tunnelCASecret,
	}

	component, err := render.APIServer(&apiServerCfg)
	if err != nil {
		r.SetDegraded(operatorv1.ResourceRenderingError, "Error rendering APIServer", err, reqLogger)
		return reconcile.Result{}, err
	}
	components := []render.Component{
		component,
		rcertificatemanagement.CertificateManagement(&rcertificatemanagement.Config{
			Namespace:       rmeta.APIServerNamespace(variant),
			ServiceAccounts: []string{render.ApiServerServiceAccountName(variant)},
			KeyPairOptions: []rcertificatemanagement.KeyPairOption{
				rcertificatemanagement.NewKeyPairOption(tlsSecret, true, true),
				rcertificatemanagement.NewKeyPairOption(tunnelCASecret, true, true),
			},
		}),
	}
	if tunnelSecretPassthrough != nil {
		components = append(components, tunnelSecretPassthrough)
	}

	if variant == operatorv1.TigeraSecureEnterprise {
		packetCaptureCertSecret, err := certificateManager.GetOrCreateKeyPair(
			r.client,
			render.PacketCaptureCertSecret,
			common.OperatorNamespace(),
			dns.GetServiceDNSNames(render.PacketCaptureServiceName, render.PacketCaptureNamespace, r.clusterDomain))
		if err != nil {
			r.SetDegraded(operatorv1.ResourceReadError, "Error retrieve or creating packet capture TLS certificate", err, reqLogger)
			return reconcile.Result{}, err
		}

		// Fetch the Authentication spec. If present, we use to configure user authentication.
		authenticationCR, err := utils.GetAuthentication(ctx, r.client)
		if err != nil && !errors.IsNotFound(err) {
			r.SetDegraded(operatorv1.ResourceNotFound, "Error querying Authentication", err, reqLogger)
			return reconcile.Result{}, err
		}

		keyValidatorConfig, err := utils.GetKeyValidatorConfig(ctx, r.client, authenticationCR, r.clusterDomain)
		if err != nil {
			r.SetDegraded(operatorv1.ResourceUpdateError, "Failed to process the authentication CR.", err, reqLogger)
			return reconcile.Result{}, err
		}

		packetCaptureApiCfg := &render.PacketCaptureApiConfiguration{
			PullSecrets:        pullSecrets,
			Openshift:          r.provider == operatorv1.ProviderOpenShift,
			Installation:       network,
			KeyValidatorConfig: keyValidatorConfig,
			ServerCertSecret:   packetCaptureCertSecret,
			ClusterDomain:      r.clusterDomain,
		}
		var pc = render.PacketCaptureAPI(packetCaptureApiCfg)
		components = append(components, pc,
			rcertificatemanagement.CertificateManagement(&rcertificatemanagement.Config{
				Namespace:       render.PacketCaptureNamespace,
				ServiceAccounts: []string{render.PacketCaptureServiceAccountName},
				KeyPairOptions: []rcertificatemanagement.KeyPairOption{
					rcertificatemanagement.NewKeyPairOption(packetCaptureCertSecret, true, true),
				},
			}),
		)
		certificateManager.AddToStatusManager(r.status, render.PacketCaptureNamespace)
	}

	if err = imageset.ApplyImageSet(ctx, r.client, variant, components...); err != nil {
		r.SetDegraded(operatorv1.ResourceUpdateError, "Error with images from ImageSet", err, reqLogger)
		return reconcile.Result{}, err
	}

	for _, component := range components {
		if err := handler.CreateOrUpdateOrDelete(context.Background(), component, r.status); err != nil {
			r.SetDegraded(operatorv1.ResourceUpdateError, "Error creating / updating resource", err, reqLogger)
			return reconcile.Result{}, err
		}
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
func (r *ReconcileAPIServer) SetDegraded(reason operatorv1.TigeraStatusReason, message string, err error, log logr.Logger) {
	log.WithValues(string(reason), message).Error(err, string(reason))
	errormsg := ""
	if err != nil {
		errormsg = err.Error()
	}
	r.status.SetDegraded(string(reason), fmt.Sprintf("%s - Error: %s", message, errormsg))
}

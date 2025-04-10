package main

// We currently accept the risk of exposing pprof and rely on users to protect the endpoint.
import (
	"context"
	"errors"
	"flag"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/kyverno/kyverno/cmd/internal"
	"github.com/kyverno/kyverno/pkg/auth/checker"
	"github.com/kyverno/kyverno/pkg/breaker"
	"github.com/kyverno/kyverno/pkg/client/clientset/versioned"
	kyvernoinformer "github.com/kyverno/kyverno/pkg/client/informers/externalversions"
	"github.com/kyverno/kyverno/pkg/clients/dclient"
	"github.com/kyverno/kyverno/pkg/config"
	"github.com/kyverno/kyverno/pkg/controllers/certmanager"
	genericloggingcontroller "github.com/kyverno/kyverno/pkg/controllers/generic/logging"
	genericwebhookcontroller "github.com/kyverno/kyverno/pkg/controllers/generic/webhook"
	globalcontextcontroller "github.com/kyverno/kyverno/pkg/controllers/globalcontext"
	policymetricscontroller "github.com/kyverno/kyverno/pkg/controllers/metrics/policy"
	policycachecontroller "github.com/kyverno/kyverno/pkg/controllers/policycache"
	vapcontroller "github.com/kyverno/kyverno/pkg/controllers/validatingadmissionpolicy-generate"
	webhookcontroller "github.com/kyverno/kyverno/pkg/controllers/webhook"
	"github.com/kyverno/kyverno/pkg/engine/apicall"
	"github.com/kyverno/kyverno/pkg/event"
	"github.com/kyverno/kyverno/pkg/globalcontext/store"
	"github.com/kyverno/kyverno/pkg/informers"
	"github.com/kyverno/kyverno/pkg/leaderelection"
	"github.com/kyverno/kyverno/pkg/logging"
	"github.com/kyverno/kyverno/pkg/policycache"
	"github.com/kyverno/kyverno/pkg/tls"
	"github.com/kyverno/kyverno/pkg/toggle"
	"github.com/kyverno/kyverno/pkg/utils/generator"
	kubeutils "github.com/kyverno/kyverno/pkg/utils/kube"
	runtimeutils "github.com/kyverno/kyverno/pkg/utils/runtime"
	"github.com/kyverno/kyverno/pkg/validatingadmissionpolicy"
	"github.com/kyverno/kyverno/pkg/validation/exception"
	"github.com/kyverno/kyverno/pkg/webhooks"
	webhooksexception "github.com/kyverno/kyverno/pkg/webhooks/exception"
	webhooksglobalcontext "github.com/kyverno/kyverno/pkg/webhooks/globalcontext"
	webhookspolicy "github.com/kyverno/kyverno/pkg/webhooks/policy"
	webhooksresource "github.com/kyverno/kyverno/pkg/webhooks/resource"
	webhookgenerate "github.com/kyverno/kyverno/pkg/webhooks/updaterequest"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiserver "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	kubeinformers "k8s.io/client-go/informers"
	appsv1informers "k8s.io/client-go/informers/apps/v1"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	kyamlopenapi "sigs.k8s.io/kustomize/kyaml/openapi"
)

const (
	exceptionWebhookControllerName   = "exception-webhook-controller"
	gctxWebhookControllerName        = "global-context-webhook-controller"
	webhookControllerFinalizerName   = "kyverno.io/webhooks"
	exceptionControllerFinalizerName = "kyverno.io/exceptionwebhooks"
	gctxControllerFinalizerName      = "kyverno.io/globalcontextwebhooks"
)

var (
	caSecretName  string
	tlsSecretName string
)

func showWarnings(ctx context.Context, logger logr.Logger) {
	logger = logger.WithName("warnings")
	// log if `forceFailurePolicyIgnore` flag has been set or not
	if toggle.FromContext(ctx).ForceFailurePolicyIgnore() {
		logger.Info("'ForceFailurePolicyIgnore' is enabled, all policies with policy failures will be set to Ignore")
	}
}

func sanityChecks(apiserverClient apiserver.Interface) error {
	return kubeutils.CRDsInstalled(apiserverClient, "clusterpolicies.kyverno.io", "policies.kyverno.io")
}

func createNonLeaderControllers(
	kyvernoInformer kyvernoinformer.SharedInformerFactory,
	dynamicClient dclient.Interface,
	policyCache policycache.Cache,
) ([]internal.Controller, func(context.Context) error) {
	policyCacheController := policycachecontroller.NewController(
		dynamicClient,
		policyCache,
		kyvernoInformer.Kyverno().V1().ClusterPolicies(),
		kyvernoInformer.Kyverno().V1().Policies(),
	)
	return []internal.Controller{
			internal.NewController(policycachecontroller.ControllerName, policyCacheController, policycachecontroller.Workers),
		},
		func(ctx context.Context) error {
			if err := policyCacheController.WarmUp(); err != nil {
				return err
			}
			return nil
		}
}

func createrLeaderControllers(
	generateVAPs bool,
	admissionReports bool,
	serverIP string,
	webhookTimeout int,
	autoUpdateWebhooks bool,
	autoDeleteWebhooks bool,
	kubeInformer kubeinformers.SharedInformerFactory,
	kubeKyvernoInformer kubeinformers.SharedInformerFactory,
	kyvernoInformer kyvernoinformer.SharedInformerFactory,
	caInformer corev1informers.SecretInformer,
	tlsInformer corev1informers.SecretInformer,
	deploymentInformer appsv1informers.DeploymentInformer,
	kubeClient kubernetes.Interface,
	kyvernoClient versioned.Interface,
	dynamicClient dclient.Interface,
	certRenewer tls.CertRenewer,
	runtime runtimeutils.Runtime,
	servicePort int32,
	webhookServerPort int32,
	configuration config.Configuration,
	eventGenerator event.Interface,
) ([]internal.Controller, func(context.Context) error, error) {
	var leaderControllers []internal.Controller
	certManager := certmanager.NewController(
		caInformer,
		tlsInformer,
		certRenewer,
		caSecretName,
		tlsSecretName,
		config.KyvernoNamespace(),
	)
	webhookController := webhookcontroller.NewController(
		dynamicClient.Discovery(),
		kubeClient.AdmissionregistrationV1().MutatingWebhookConfigurations(),
		kubeClient.AdmissionregistrationV1().ValidatingWebhookConfigurations(),
		kubeClient.CoordinationV1().Leases(config.KyvernoNamespace()),
		kyvernoClient,
		kubeInformer.Admissionregistration().V1().MutatingWebhookConfigurations(),
		kubeInformer.Admissionregistration().V1().ValidatingWebhookConfigurations(),
		kyvernoInformer.Kyverno().V1().ClusterPolicies(),
		kyvernoInformer.Kyverno().V1().Policies(),
		deploymentInformer,
		caInformer,
		kubeKyvernoInformer.Coordination().V1().Leases(),
		kubeInformer.Rbac().V1().ClusterRoles(),
		kyvernoInformer.Kyverno().V2alpha1().GlobalContextEntries(),
		serverIP,
		int32(webhookTimeout), //nolint:gosec
		servicePort,
		webhookServerPort,
		autoUpdateWebhooks,
		autoDeleteWebhooks,
		admissionReports,
		runtime,
		configuration,
		caSecretName,
		webhookcontroller.WebhookCleanupSetup(kubeClient, webhookControllerFinalizerName),
		webhookcontroller.WebhookCleanupHandler(kubeClient, webhookControllerFinalizerName),
	)
	exceptionWebhookController := genericwebhookcontroller.NewController(
		exceptionWebhookControllerName,
		kubeClient.AdmissionregistrationV1().ValidatingWebhookConfigurations(),
		kubeInformer.Admissionregistration().V1().ValidatingWebhookConfigurations(),
		caInformer,
		deploymentInformer,
		config.ExceptionValidatingWebhookConfigurationName,
		config.ExceptionValidatingWebhookServicePath,
		serverIP,
		servicePort,
		webhookServerPort,
		nil,
		[]admissionregistrationv1.RuleWithOperations{{
			Rule: admissionregistrationv1.Rule{
				APIGroups:   []string{"kyverno.io"},
				APIVersions: []string{"v2alpha1", "v2beta1"},
				Resources:   []string{"policyexceptions"},
			},
			Operations: []admissionregistrationv1.OperationType{
				admissionregistrationv1.Create,
				admissionregistrationv1.Update,
			},
		}},
		genericwebhookcontroller.Fail,
		genericwebhookcontroller.None,
		configuration,
		caSecretName,
		runtime,
		autoDeleteWebhooks,
		webhookcontroller.WebhookCleanupSetup(kubeClient, exceptionControllerFinalizerName),
		webhookcontroller.WebhookCleanupHandler(kubeClient, exceptionControllerFinalizerName),
	)
	gctxWebhookController := genericwebhookcontroller.NewController(
		gctxWebhookControllerName,
		kubeClient.AdmissionregistrationV1().ValidatingWebhookConfigurations(),
		kubeInformer.Admissionregistration().V1().ValidatingWebhookConfigurations(),
		caInformer,
		deploymentInformer,
		config.GlobalContextValidatingWebhookConfigurationName,
		config.GlobalContextValidatingWebhookServicePath,
		serverIP,
		servicePort,
		webhookServerPort,
		nil,
		[]admissionregistrationv1.RuleWithOperations{{
			Rule: admissionregistrationv1.Rule{
				APIGroups:   []string{"kyverno.io"},
				APIVersions: []string{"v2alpha1"},
				Resources:   []string{"globalcontextentries"},
			},
			Operations: []admissionregistrationv1.OperationType{
				admissionregistrationv1.Create,
				admissionregistrationv1.Update,
			},
		}},
		genericwebhookcontroller.Fail,
		genericwebhookcontroller.None,
		configuration,
		caSecretName,
		runtime,
		autoDeleteWebhooks,
		webhookcontroller.WebhookCleanupSetup(kubeClient, gctxControllerFinalizerName),
		webhookcontroller.WebhookCleanupHandler(kubeClient, gctxControllerFinalizerName),
	)
	leaderControllers = append(leaderControllers, internal.NewController(certmanager.ControllerName, certManager, certmanager.Workers))
	leaderControllers = append(leaderControllers, internal.NewController(webhookcontroller.ControllerName, webhookController, webhookcontroller.Workers))
	leaderControllers = append(leaderControllers, internal.NewController(exceptionWebhookControllerName, exceptionWebhookController, 1))
	leaderControllers = append(leaderControllers, internal.NewController(gctxWebhookControllerName, gctxWebhookController, 1))

	if generateVAPs {
		checker := checker.NewSelfChecker(kubeClient.AuthorizationV1().SelfSubjectAccessReviews())
		vapController := vapcontroller.NewController(
			kubeClient,
			kyvernoClient,
			dynamicClient.Discovery(),
			kyvernoInformer.Kyverno().V1().ClusterPolicies(),
			kyvernoInformer.Kyverno().V2().PolicyExceptions(),
			kubeInformer.Admissionregistration().V1beta1().ValidatingAdmissionPolicies(),
			kubeInformer.Admissionregistration().V1beta1().ValidatingAdmissionPolicyBindings(),
			eventGenerator,
			checker,
		)
		leaderControllers = append(leaderControllers, internal.NewController(vapcontroller.ControllerName, vapController, vapcontroller.Workers))
	}
	return leaderControllers, nil, nil
}

func main() {
	var (
		// TODO: this has been added to backward support command line arguments
		// will be removed in future and the configuration will be set only via configmaps
		serverIP                     string
		webhookTimeout               int
		maxQueuedEvents              int
		omitEvents                   string
		autoUpdateWebhooks           bool
		autoDeleteWebhooks           bool
		webhookRegistrationTimeout   time.Duration
		admissionReports             bool
		dumpPayload                  bool
		servicePort                  int
		webhookServerPort            int
		backgroundServiceAccountName string
		reportsServiceAccountName    string
		maxAPICallResponseLength     int64
		renewBefore                  time.Duration
		maxAuditWorkers              int
		maxAuditCapacity             int
		maxAdmissionReports          int
	)
	flagset := flag.NewFlagSet("kyverno", flag.ExitOnError)
	flagset.BoolVar(&dumpPayload, "dumpPayload", false, "Set this flag to activate/deactivate debug mode.")
	flagset.IntVar(&webhookTimeout, "webhookTimeout", webhookcontroller.DefaultWebhookTimeout, "Timeout for webhook configurations (number of seconds, integer).")
	flagset.IntVar(&maxQueuedEvents, "maxQueuedEvents", 1000, "Maximum events to be queued.")
	flagset.StringVar(&omitEvents, "omitEvents", "", "Set this flag to a comma sperated list of PolicyViolation, PolicyApplied, PolicyError, PolicySkipped to disable events, e.g. --omitEvents=PolicyApplied,PolicyViolation")
	flagset.StringVar(&serverIP, "serverIP", "", "IP address where Kyverno controller runs. Only required if out-of-cluster.")
	flagset.BoolVar(&autoUpdateWebhooks, "autoUpdateWebhooks", true, "Set this flag to 'false' to disable auto-configuration of the webhook.")
	flagset.BoolVar(&autoDeleteWebhooks, "autoDeleteWebhooks", false, "Set this flag to 'true' to enable autodeletion of webhook configurations using finalizers (requires extra permissions).")
	flagset.DurationVar(&webhookRegistrationTimeout, "webhookRegistrationTimeout", 120*time.Second, "Timeout for webhook registration, e.g., 30s, 1m, 5m.")
	flagset.Func(toggle.ProtectManagedResourcesFlagName, toggle.ProtectManagedResourcesDescription, toggle.ProtectManagedResources.Parse)
	flagset.Func(toggle.ForceFailurePolicyIgnoreFlagName, toggle.ForceFailurePolicyIgnoreDescription, toggle.ForceFailurePolicyIgnore.Parse)
	flagset.Func(toggle.GenerateValidatingAdmissionPolicyFlagName, toggle.GenerateValidatingAdmissionPolicyDescription, toggle.GenerateValidatingAdmissionPolicy.Parse)
	flagset.Func(toggle.DumpMutatePatchesFlagName, toggle.DumpMutatePatchesDescription, toggle.DumpMutatePatches.Parse)
	flagset.BoolVar(&admissionReports, "admissionReports", true, "Enable or disable admission reports.")
	flagset.IntVar(&servicePort, "servicePort", 443, "Port used by the Kyverno Service resource and for webhook configurations.")
	flagset.IntVar(&webhookServerPort, "webhookServerPort", 9443, "Port used by the webhook server.")
	flagset.StringVar(&backgroundServiceAccountName, "backgroundServiceAccountName", "", "Background controller service account name.")
	flagset.StringVar(&reportsServiceAccountName, "reportsServiceAccountName", "", "Reports controller service account name.")
	flagset.StringVar(&caSecretName, "caSecretName", "", "Name of the secret containing CA.")
	flagset.StringVar(&tlsSecretName, "tlsSecretName", "", "Name of the secret containing TLS pair.")
	flagset.Int64Var(&maxAPICallResponseLength, "maxAPICallResponseLength", 10*1000*1000, "Configure the value of maximum allowed GET response size from API Calls")
	flagset.DurationVar(&renewBefore, "renewBefore", 15*24*time.Hour, "The certificate renewal time before expiration")
	flagset.IntVar(&maxAuditWorkers, "maxAuditWorkers", 8, "Maximum number of workers for audit policy processing")
	flagset.IntVar(&maxAuditCapacity, "maxAuditCapacity", 1000, "Maximum capacity of the audit policy task queue")
	flagset.IntVar(&maxAdmissionReports, "maxAdmissionReports", 10000, "Maximum number of admission reports before we stop creating new ones")
	// config
	appConfig := internal.NewConfiguration(
		internal.WithProfiling(),
		internal.WithTracing(),
		internal.WithMetrics(),
		internal.WithKubeconfig(),
		internal.WithPolicyExceptions(),
		internal.WithConfigMapCaching(),
		internal.WithDeferredLoading(),
		internal.WithCosign(),
		internal.WithRegistryClient(),
		internal.WithImageVerifyCache(),
		internal.WithLeaderElection(),
		internal.WithKyvernoClient(),
		internal.WithDynamicClient(),
		internal.WithKyvernoDynamicClient(),
		internal.WithEventsClient(),
		internal.WithApiServerClient(),
		internal.WithMetadataClient(),
		internal.WithFlagSets(flagset),
		internal.WithReporting(),
	)
	// parse flags
	internal.ParseFlags(appConfig)
	var wg sync.WaitGroup
	func() {
		// setup
		signalCtx, setup, sdown := internal.Setup(appConfig, "kyverno-admission-controller", false)
		defer sdown()
		if caSecretName == "" {
			setup.Logger.Error(errors.New("exiting... caSecretName is a required flag"), "exiting... caSecretName is a required flag")
			os.Exit(1)
		}
		if tlsSecretName == "" {
			setup.Logger.Error(errors.New("exiting... tlsSecretName is a required flag"), "exiting... tlsSecretName is a required flag")
			os.Exit(1)
		}
		// check if validating admission policies are registered in the API server
		generateValidatingAdmissionPolicy := toggle.FromContext(context.TODO()).GenerateValidatingAdmissionPolicy()
		if generateValidatingAdmissionPolicy {
			registered, err := validatingadmissionpolicy.IsValidatingAdmissionPolicyRegistered(setup.KubeClient)
			if !registered {
				setup.Logger.Error(err, "ValidatingAdmissionPolicies isn't supported in the API server")
				os.Exit(1)
			}
		}
		caSecret := informers.NewSecretInformer(setup.KubeClient, config.KyvernoNamespace(), caSecretName, setup.ResyncPeriod)
		tlsSecret := informers.NewSecretInformer(setup.KubeClient, config.KyvernoNamespace(), tlsSecretName, setup.ResyncPeriod)
		kyvernoDeployment := informers.NewDeploymentInformer(setup.KubeClient, config.KyvernoNamespace(), config.KyvernoDeploymentName(), setup.ResyncPeriod)
		if !informers.StartInformersAndWaitForCacheSync(signalCtx, setup.Logger, caSecret, tlsSecret, kyvernoDeployment) {
			setup.Logger.Error(errors.New("failed to wait for cache sync"), "failed to wait for cache sync")
			os.Exit(1)
		}
		// show version
		showWarnings(signalCtx, setup.Logger)
		// THIS IS AN UGLY FIX
		// ELSE KYAML IS NOT THREAD SAFE
		kyamlopenapi.Schema()
		// check we can run
		if err := sanityChecks(setup.ApiServerClient); err != nil {
			setup.Logger.Error(err, "sanity checks failed")
			os.Exit(1)
		}
		// informer factories
		kubeInformer := kubeinformers.NewSharedInformerFactory(setup.KubeClient, setup.ResyncPeriod)
		kubeKyvernoInformer := kubeinformers.NewSharedInformerFactoryWithOptions(setup.KubeClient, setup.ResyncPeriod, kubeinformers.WithNamespace(config.KyvernoNamespace()))
		kyvernoInformer := kyvernoinformer.NewSharedInformerFactory(setup.KyvernoClient, setup.ResyncPeriod)

		certRenewer := tls.NewCertRenewer(
			setup.KubeClient.CoreV1().Secrets(config.KyvernoNamespace()),
			tls.CertRenewalInterval,
			tls.CAValidityDuration,
			tls.TLSValidityDuration,
			renewBefore,
			serverIP,
			config.KyvernoServiceName(),
			config.DnsNames(config.KyvernoServiceName(), config.KyvernoNamespace()),
			config.KyvernoNamespace(),
			caSecretName,
			tlsSecretName,
		)
		policyCache := policycache.NewCache()
		eventGenerator := event.NewEventGenerator(
			setup.EventsClient,
			logging.WithName("EventGenerator"),
			maxQueuedEvents,
			strings.Split(omitEvents, ",")...,
		)
		gcstore := store.New()
		gceController := internal.NewController(
			globalcontextcontroller.ControllerName,
			globalcontextcontroller.NewController(
				kyvernoInformer.Kyverno().V2alpha1().GlobalContextEntries(),
				setup.KyvernoDynamicClient,
				setup.KyvernoClient,
				gcstore,
				eventGenerator,
				maxAPICallResponseLength,
				true,
			),
			globalcontextcontroller.Workers,
		)
		polexCache, polexController := internal.NewExceptionSelector(setup.Logger, kyvernoInformer)
		eventController := internal.NewController(
			event.ControllerName,
			eventGenerator,
			event.Workers,
		)
		// this controller only subscribe to events, nothing is returned...
		policymetricscontroller.NewController(
			setup.MetricsManager,
			kyvernoInformer.Kyverno().V1().ClusterPolicies(),
			kyvernoInformer.Kyverno().V1().Policies(),
			&wg,
		)
		// log policy changes
		genericloggingcontroller.NewController(
			setup.Logger.WithName("policy"),
			"Policy",
			kyvernoInformer.Kyverno().V1().Policies(),
			genericloggingcontroller.CheckGeneration,
		)
		genericloggingcontroller.NewController(
			setup.Logger.WithName("cluster-policy"),
			"ClusterPolicy",
			kyvernoInformer.Kyverno().V1().ClusterPolicies(),
			genericloggingcontroller.CheckGeneration,
		)
		runtime := runtimeutils.NewRuntime(
			setup.Logger.WithName("runtime-checks"),
			serverIP,
			kubeKyvernoInformer.Apps().V1().Deployments(),
			certRenewer,
		)
		// engine
		engine := internal.NewEngine(
			signalCtx,
			setup.Logger,
			setup.Configuration,
			setup.MetricsConfiguration,
			setup.Jp,
			setup.KyvernoDynamicClient,
			setup.RegistryClient,
			setup.ImageVerifyCacheClient,
			setup.KubeClient,
			setup.KyvernoClient,
			setup.RegistrySecretLister,
			apicall.NewAPICallConfiguration(maxAPICallResponseLength),
			polexCache,
			gcstore,
		)
		// create non leader controllers
		nonLeaderControllers, nonLeaderBootstrap := createNonLeaderControllers(
			kyvernoInformer,
			setup.KyvernoDynamicClient,
			policyCache,
		)
		// start informers and wait for cache sync
		if !internal.StartInformersAndWaitForCacheSync(signalCtx, setup.Logger, kyvernoInformer, kubeInformer, kubeKyvernoInformer) {
			setup.Logger.Error(errors.New("failed to wait for cache sync"), "failed to wait for cache sync")
			os.Exit(1)
		}
		// bootstrap non leader controllers
		if nonLeaderBootstrap != nil {
			if err := nonLeaderBootstrap(signalCtx); err != nil {
				setup.Logger.Error(err, "failed to bootstrap non leader controllers")
				os.Exit(1)
			}
		}
		// setup leader election
		le, err := leaderelection.New(
			setup.Logger.WithName("leader-election"),
			"kyverno",
			config.KyvernoNamespace(),
			setup.LeaderElectionClient,
			config.KyvernoPodName(),
			internal.LeaderElectionRetryPeriod(),
			func(ctx context.Context) {
				logger := setup.Logger.WithName("leader")
				// create leader factories
				kubeInformer := kubeinformers.NewSharedInformerFactory(setup.KubeClient, setup.ResyncPeriod)
				kyvernoInformer := kyvernoinformer.NewSharedInformerFactory(setup.KyvernoClient, setup.ResyncPeriod)
				// create leader controllers
				leaderControllers, warmup, err := createrLeaderControllers(
					generateValidatingAdmissionPolicy,
					admissionReports,
					serverIP,
					webhookTimeout,
					autoUpdateWebhooks,
					autoDeleteWebhooks,
					kubeInformer,
					kubeKyvernoInformer,
					kyvernoInformer,
					caSecret,
					tlsSecret,
					kyvernoDeployment,
					setup.KubeClient,
					setup.KyvernoClient,
					setup.KyvernoDynamicClient,
					certRenewer,
					runtime,
					int32(servicePort),       //nolint:gosec
					int32(webhookServerPort), //nolint:gosec
					setup.Configuration,
					eventGenerator,
				)
				if err != nil {
					logger.Error(err, "failed to create leader controllers")
					os.Exit(1)
				}
				// start informers and wait for cache sync
				if !internal.StartInformersAndWaitForCacheSync(signalCtx, logger, kyvernoInformer, kubeInformer, kubeKyvernoInformer) {
					logger.Error(errors.New("failed to wait for cache sync"), "failed to wait for cache sync")
					os.Exit(1)
				}
				if warmup != nil {
					if err := warmup(ctx); err != nil {
						logger.Error(err, "failed to run warmup")
						os.Exit(1)
					}
				}
				// start leader controllers
				var wg sync.WaitGroup
				for _, controller := range leaderControllers {
					controller.Run(signalCtx, logger.WithName("controllers"), &wg)
				}
				// wait all controllers shut down
				wg.Wait()
			},
			nil,
		)
		if err != nil {
			setup.Logger.Error(err, "failed to initialize leader election")
			os.Exit(1)
		}
		urGenerator := generator.NewUpdateRequestGenerator(setup.Configuration, setup.MetadataClient)
		// create webhooks server
		urgen := webhookgenerate.NewGenerator(
			setup.KyvernoClient,
			kyvernoInformer.Kyverno().V2().UpdateRequests(),
			urGenerator,
		)
		policyHandlers := webhookspolicy.NewHandlers(
			setup.KyvernoDynamicClient,
			setup.KyvernoClient,
			backgroundServiceAccountName,
			reportsServiceAccountName,
		)
		ephrs, err := breaker.StartAdmissionReportsCounter(signalCtx, setup.MetadataClient)
		if err != nil {
			setup.Logger.Error(err, "failed to start admission reports watcher")
			os.Exit(1)
		}
		reportsBreaker := breaker.NewBreaker("admission reports", func(context.Context) bool {
			count, isRunning := ephrs.Count()
			if !isRunning {
				return true
			}
			return count > maxAdmissionReports
		})
		resourceHandlers := webhooksresource.NewHandlers(
			engine,
			setup.KyvernoDynamicClient,
			setup.KyvernoClient,
			setup.Configuration,
			setup.MetricsManager,
			policyCache,
			kubeInformer.Core().V1().Namespaces().Lister(),
			kyvernoInformer.Kyverno().V2().UpdateRequests().Lister().UpdateRequests(config.KyvernoNamespace()),
			kyvernoInformer.Kyverno().V1().ClusterPolicies(),
			kyvernoInformer.Kyverno().V1().Policies(),
			urgen,
			eventGenerator,
			admissionReports,
			backgroundServiceAccountName,
			reportsServiceAccountName,
			setup.Jp,
			maxAuditWorkers,
			maxAuditCapacity,
			setup.ReportingConfiguration,
			reportsBreaker,
		)
		exceptionHandlers := webhooksexception.NewHandlers(exception.ValidationOptions{
			Enabled:   internal.PolicyExceptionEnabled(),
			Namespace: internal.ExceptionNamespace(),
		})
		globalContextHandlers := webhooksglobalcontext.NewHandlers()
		server := webhooks.NewServer(
			signalCtx,
			policyHandlers,
			resourceHandlers,
			exceptionHandlers,
			globalContextHandlers,
			setup.Configuration,
			setup.MetricsManager,
			webhooks.DebugModeOptions{
				DumpPayload: dumpPayload,
			},
			func() ([]byte, []byte, error) {
				secret, err := tlsSecret.Lister().Secrets(config.KyvernoNamespace()).Get(tlsSecretName)
				if err != nil {
					return nil, nil, err
				}
				return secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey], nil
			},
			setup.KubeClient.AdmissionregistrationV1().MutatingWebhookConfigurations(),
			setup.KubeClient.AdmissionregistrationV1().ValidatingWebhookConfigurations(),
			setup.KubeClient.CoordinationV1().Leases(config.KyvernoNamespace()),
			runtime,
			kubeInformer.Rbac().V1().RoleBindings().Lister(),
			kubeInformer.Rbac().V1().ClusterRoleBindings().Lister(),
			setup.KyvernoDynamicClient.Discovery(),
			int32(webhookServerPort), //nolint:gosec
		)
		// start informers and wait for cache sync
		// we need to call start again because we potentially registered new informers
		if !internal.StartInformersAndWaitForCacheSync(signalCtx, setup.Logger, kyvernoInformer, kubeInformer, kubeKyvernoInformer) {
			setup.Logger.Error(errors.New("failed to wait for cache sync"), "failed to wait for cache sync")
			os.Exit(1)
		}
		// start webhooks server
		server.Run()
		defer server.Stop()
		// start non leader controllers
		eventController.Run(signalCtx, setup.Logger, &wg)
		gceController.Run(signalCtx, setup.Logger, &wg)
		if polexController != nil {
			polexController.Run(signalCtx, setup.Logger, &wg)
		}
		for _, controller := range nonLeaderControllers {
			controller.Run(signalCtx, setup.Logger.WithName("controllers"), &wg)
		}
		// start leader election
		le.Run(signalCtx)
	}()
	// wait for everything to shut down and exit
	wg.Wait()
}

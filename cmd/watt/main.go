package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/datawire/teleproxy/pkg/k8s"
	"github.com/datawire/teleproxy/pkg/limiter"
	"github.com/datawire/teleproxy/pkg/supervisor"
	"github.com/spf13/cobra"
)

var kubernetesNamespace string
var initialSources = make([]string, 0)
var initialFieldSelector string
var initialLabelSelector string
var watchHooks = make([]string, 0)
var notifyReceivers = make([]string, 0)
var port int
var interval time.Duration

var rootCmd = &cobra.Command{
	Use:              "watt",
	Short:            "watt",
	Long:             "watt - watch all the things",
	PersistentPreRun: func(cmd *cobra.Command, args []string) {},
	Run:              runWatt,
}

func init() {
	rootCmd.Flags().StringVarP(&kubernetesNamespace, "namespace", "n", "", "namespace to watch (default: all)")
	rootCmd.Flags().StringSliceVarP(&initialSources, "source", "s", []string{}, "configure an initial static source")
	rootCmd.Flags().StringVar(&initialFieldSelector, "fields", "", "configure an initial field selector string")
	rootCmd.Flags().StringVar(&initialLabelSelector, "labels", "", "configure an initial label selector string")
	rootCmd.Flags().StringSliceVarP(&watchHooks, "watch", "w", []string{}, "configure watch hook(s)")
	rootCmd.Flags().StringSliceVar(&notifyReceivers, "notify", []string{},
		"invoke the program with the given arguments as a receiver")
	rootCmd.Flags().IntVarP(&port, "port", "p", 7000, "configure the snapshot server port")
	rootCmd.Flags().DurationVarP(&interval, "interval", "i", 250*time.Millisecond,
		"configure the rate limit interval")
}

func runWatt(cmd *cobra.Command, args []string) {
	os.Exit(_runWatt(cmd, args))
}

func _runWatt(cmd *cobra.Command, args []string) int {
	if len(initialSources) == 0 {
		log.Println("no initial sources configured")
		return 1
	}

	// XXX: we don't need to create this here anymore
	client := k8s.NewClient(nil)
	kubeAPIWatcher := client.Watcher()
	/*for idx := range initialSources {
		initialSources[idx] = kubeAPIWatcher.Canonical(initialSources[idx])
	}*/

	log.Printf("starting watt...")

	// The aggregator sends the current consul resolver set to the
	// consul watch manager.
	aggregatorToConsulwatchmanCh := make(chan []ConsulWatchSpec)

	// The aggregator sends the current k8s watch set to the
	// kubernetes watch manager.
	aggregatorToKubewatchmanCh := make(chan []KubernetesWatchSpec)

	invoker := NewInvoker(port, notifyReceivers)
	limiter := limiter.NewComposite(limiter.NewUnlimited(), limiter.NewInterval(interval), interval)
	aggregator := NewAggregator(invoker.Snapshots, aggregatorToKubewatchmanCh, aggregatorToConsulwatchmanCh,
		initialSources, ExecWatchHook(watchHooks), limiter)

	kubebootstrap := kubebootstrap{
		namespace:      kubernetesNamespace,
		kinds:          initialSources,
		fieldSelector:  initialFieldSelector,
		labelSelector:  initialLabelSelector,
		kubeAPIWatcher: kubeAPIWatcher,
		notify:         []chan<- k8sEvent{aggregator.KubernetesEvents},
	}

	consulwatchman := consulwatchman{
		WatchMaker: &ConsulWatchMaker{aggregatorCh: aggregator.ConsulEvents},
		watchesCh:  aggregatorToConsulwatchmanCh,
		watched:    make(map[string]*supervisor.Worker),
	}

	kubewatchman := kubewatchman{
		WatchMaker: &KubernetesWatchMaker{kubeAPI: client, notify: aggregator.KubernetesEvents},
		in:         aggregatorToKubewatchmanCh,
	}

	apiServer := &apiServer{
		port:    port,
		invoker: invoker,
	}

	ctx := context.Background()
	s := supervisor.WithContext(ctx)

	s.Supervise(&supervisor.Worker{
		Name: "kubebootstrap",
		Work: kubebootstrap.Work,
	})

	s.Supervise(&supervisor.Worker{
		Name: "consulwatchman",
		Work: consulwatchman.Work,
	})

	s.Supervise(&supervisor.Worker{
		Name: "kubewatchman",
		Work: kubewatchman.Work,
	})

	s.Supervise(&supervisor.Worker{
		Name: "aggregator",
		Work: aggregator.Work,
	})

	s.Supervise(&supervisor.Worker{
		Name: "invoker",
		Work: invoker.Work,
	})

	s.Supervise(&supervisor.Worker{
		Name: "api",
		Work: apiServer.Work,
	})

	if errs := s.Run(); len(errs) > 0 {
		for _, err := range errs {
			log.Println(err)
		}
		return 1
	}

	return 0
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

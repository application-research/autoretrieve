package metrics

import (
	"net/http"

	"contrib.go.opencensus.io/exporter/prometheus"
	logging "github.com/ipfs/go-log/v2"
	"go.opencensus.io/stats/view"
)

var logger = logging.Logger("metrics")

func PrometheusHandler(views ...*view.View) http.Handler {
	if err := view.Register(DefaultViews...); err != nil {
		logger.Errorf("cannot register default metric views: %s", err)
	}

	if err := view.Register(views...); err != nil {
		logger.Errorf("cannot register metric views: %s", err)
	}

	exp, err := prometheus.NewExporter(prometheus.Options{
		Namespace: "autoretrieve",
	})
	if err != nil {
		logger.Errorf("cannot create the prometheus stats exporter: %v", err)
	}

	return exp
}

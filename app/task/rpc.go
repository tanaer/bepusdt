package task

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/v03413/bepusdt/app/conf"
	"github.com/v03413/bepusdt/app/log"
	"github.com/v03413/bepusdt/app/model"
)

func reportRPCFailure(network, endpoint string, reason any) {
	conf.RecordFailure(network)

	next := model.ReportEndpointFailure(model.Network(network), endpoint)
	if log.Task == nil {
		return
	}

	if next == "" || next == endpoint {
		log.Task.Warn(fmt.Sprintf("%s RPC endpoint failed: %v", network, reason))
		return
	}

	log.Task.Warn(fmt.Sprintf(
		"%s RPC endpoint failed: %v; switching %s -> %s",
		network,
		reason,
		maskEndpoint(endpoint),
		maskEndpoint(next),
	))
}

func maskEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return endpoint
	}

	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""

	return u.String()
}

func doRPCRequestWithFailover(
	ctx context.Context,
	client *http.Client,
	network string,
	build func(endpoint string) (*http.Request, error),
	validate func(body []byte) error,
) ([]byte, string, error) {
	candidates := model.EndpointCandidates(model.Network(network))
	if len(candidates) == 0 {
		return nil, "", fmt.Errorf("%s rpc endpoint is empty", network)
	}

	var lastErr error
	for range candidates {
		endpoint := model.Endpoint(model.Network(network))
		req, err := build(endpoint)
		if err != nil {
			lastErr = err
			reportRPCFailure(network, endpoint, err)
			continue
		}

		resp, err := client.Do(req.WithContext(ctx))
		if err != nil {
			lastErr = err
			reportRPCFailure(network, endpoint, err)
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			reportRPCFailure(network, endpoint, readErr)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("status code %d", resp.StatusCode)
			reportRPCFailure(network, endpoint, lastErr)
			continue
		}
		if validate != nil {
			if err := validate(body); err != nil {
				lastErr = err
				reportRPCFailure(network, endpoint, err)
				continue
			}
		}

		conf.RecordSuccess(network, endpoint)
		return body, endpoint, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("%s rpc request failed without details", network)
	}

	return nil, "", lastErr
}

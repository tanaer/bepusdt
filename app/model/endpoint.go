package model

import (
	"regexp"
	"strings"
	"sync"
)

var endpointMu sync.Mutex
var endpointIndexes = make(map[Network]int)

var endpointSeparators = regexp.MustCompile(`[,\n;\s]+`)

func EndpointCandidates(net Network) []string {
	endpointKey, ok := networkEndpointMap[net]
	if !ok {
		return nil
	}

	raw := GetC(endpointKey)
	if raw == "" {
		raw = GetK(endpointKey)
	}

	return parseEndpoints(raw)
}

func ReportEndpointFailure(net Network, failed string) string {
	endpoints := EndpointCandidates(net)
	if len(endpoints) == 0 {
		return ""
	}
	if len(endpoints) == 1 {
		return endpoints[0]
	}

	endpointMu.Lock()
	defer endpointMu.Unlock()

	index := endpointIndexes[net]
	if index < 0 || index >= len(endpoints) || endpoints[index] == failed {
		index = (index + 1) % len(endpoints)
		endpointIndexes[net] = index
	}

	return endpoints[index]
}

func parseEndpoints(raw string) []string {
	fields := endpointSeparators.Split(strings.TrimSpace(raw), -1)
	endpoints := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if _, ok := seen[field]; ok {
			continue
		}

		seen[field] = struct{}{}
		endpoints = append(endpoints, field)
	}

	return endpoints
}

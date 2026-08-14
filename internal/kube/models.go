package kube

import "encoding/json"

type listMetadata struct {
	ResourceVersion string `json:"resourceVersion"`
	Continue        string `json:"continue"`
}

type objectMetadata struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	ResourceVersion string            `json:"resourceVersion"`
	Labels          map[string]string `json:"labels"`
}

type endpointSliceList struct {
	Metadata listMetadata    `json:"metadata"`
	Items    []endpointSlice `json:"items"`
}

type endpointSlice struct {
	Metadata    objectMetadata `json:"metadata"`
	AddressType string         `json:"addressType"`
	Ports       []endpointPort `json:"ports"`
	Endpoints   []endpoint     `json:"endpoints"`
}

type endpointPort struct {
	Name     *string `json:"name"`
	Protocol *string `json:"protocol"`
	Port     *int    `json:"port"`
}

type endpoint struct {
	Addresses  []string           `json:"addresses"`
	Conditions endpointConditions `json:"conditions"`
}

type endpointConditions struct {
	Ready *bool `json:"ready"`
}

type watchEvent struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

type statusObject struct {
	Metadata objectMetadata `json:"metadata"`
	Code     int            `json:"code"`
	Reason   string         `json:"reason"`
	Message  string         `json:"message"`
}

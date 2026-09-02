// Package videos owns the wire shapes of the OpenAI Videos API: how a
// caller submits a video generation job, and the job resource they poll
// for it. The official SDK always sends multipart/form-data for the
// create call (it forces the content type before posting), so the parser
// accepts both that and the plain JSON shape curl callers use; the job
// resource is rendered back in the exact four-value status vocabulary the
// SDK's strict typing accepts. Provider dialects that speak task-based
// native APIs of their own live beside this file, one per provider.
package videos

import (
	"encoding/json"
	"strings"
)

// CreatePath is the ingress route of the create call, and what the
// resource routes hang off: GET /v1/videos/{id} and GET /v1/videos/{id}/content.
const CreatePath = "/v1/videos"

// The seconds vocabulary the API defines. DefaultSeconds is what an
// omitted field means; the door, not the parser, judges values — a parse
// that silently rewrote a caller's "5" into a legal value would hide the
// one fact the caller needs to learn.
const (
	DefaultSeconds = 4
)

// ValidSeconds reports whether v is a duration the API defines.
func ValidSeconds(v int) bool {
	return v == 4 || v == 8 || v == 12
}

// The size vocabulary the API defines, as WIDTHxHEIGHT strings. DefaultSize
// is what an omitted field means; sizes are kept verbatim because they are
// the pricing axis of a video request, keyed by resolution tier.
const (
	DefaultSize = "720x1280"
)

// ValidSize reports whether s is a size the API defines. The empty string
// is not — an unset size is represented by the field being empty, not by
// this predicate bending to accept it.
func ValidSize(s string) bool {
	switch s {
	case "720x1280", "1280x720", "1024x1792", "1792x1024":
		return true
	}
	return false
}

// File is one uploaded file of a create call: the reference image a
// caller attaches directly instead of pointing at it by URL.
type File struct {
	FieldName   string
	FileName    string
	ContentType string
	Data        []byte
}

// InputRef is the reference image a caller may guide generation with, in
// the shapes the API allows: a URL or base64 data URL as image_url, a
// Files-API id as file_id, or — multipart only — the bytes themselves as
// File. The API says exactly one of image_url and file_id; judging which
// shapes this gateway can actually serve is the door's job, not the
// parser's.
type InputRef struct {
	ImageURL string `json:"image_url"`
	FileID   string `json:"file_id"`
	// File is set only by the multipart reader, when the input_reference
	// part carried a filename. A multipart part under the same name
	// without one is the JSON object form, Stainless-style, and lands in
	// the fields above.
	File *File
}

// inputRefJSON is the wire spelling of InputRef for the JSON body shape.
type inputRefJSON struct {
	ImageURL string `json:"image_url"`
	FileID   string `json:"file_id"`
}

// CreateRequest is the routing- and billing-relevant view of a create
// call. Seconds and Size travel as the caller wrote them — empty or zero
// meaning unset — so the door can refuse an illegal value instead of a
// rewrite making it disappear.
type CreateRequest struct {
	// Model is the name the caller used, before any candidate rewrites
	// it. The API lets callers omit it (the official service defaults to
	// its own model); this gateway routes by name, so the door requires
	// it rather than inventing a default the caller never chose.
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	// Seconds is the requested clip duration; 0 means the field was not
	// sent. Size is the requested resolution; "" means the same.
	Seconds int    `json:"seconds"`
	Size    string `json:"size"`
	// InputReference is the reference image when the caller sent one.
	InputReference *InputRef `json:"input_reference"`
}

// ParseCreateRequest decodes the routing-relevant fields of a create call
// from either body shape: multipart/form-data (what the official SDK
// always sends) or JSON (what thin clients send). Unknown fields are
// ignored by design — the video dialect is young enough that a future SDK
// adding a knob must not break older gateways, and no field this package
// models has a passthrough that unknown fields could corrupt. A body
// whose modelled fields carry the wrong types is an error, for the same
// reason as every other dialect: a request the gateway could not read
// honestly is not one it can route honestly.
func ParseCreateRequest(contentType string, body []byte) (*CreateRequest, error) {
	if strings.HasPrefix(contentType, "multipart/") {
		return parseMultipartCreate(contentType, body)
	}
	var req CreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

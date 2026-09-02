package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The bytes half: pushing a file into the Library.
//
// ===========================================================================
// TWO ROUTES, AND THE CHUNKED ONE IS THE NORMAL PATH HERE
// ===========================================================================
// Small files take POST /artifacts in one multipart request. Everything above
// the threshold opens a SESSION and streams 16 MiB chunks. The epic's own
// scenario is a folder of client video, so on this feature every file is above
// the threshold and the session route is not the exception -- a client that
// only implemented the one-shot path would work in a demo of a text folder and
// fail at the thing it was built for.
//
// THE THRESHOLD IS THE CLIENT'S CHOICE, not a server rule: the engine will
// accept either route at any size. 32 MiB matches what MemQL OS uses, and it
// is chosen against the front door's own body cap rather than against the
// engine's per-file limit.
//
// ===========================================================================
// EVERY PUSH NAMES ITS MACHINE AND ITS PATH
// ===========================================================================
// `uploadedFromWorkerId` + `uploadedFromPath` are what make a re-push a new
// VERSION of the same file rather than a duplicate: the engine resolves the
// (machine, path) key itself and needs no id from us. It also verifies the
// machine against the CALLER'S OWN FLEET before a byte moves, which is the
// gate that makes a watch row safe to be a plain client-writable preference.
//
// A half-claim is refused by the engine on purpose (a path without an id), so
// both are always sent together or neither is.

const (
	// OneShotLimitBytes is where this client switches to the session route.
	OneShotLimitBytes int64 = 32 << 20

	// uploadInitMaxBody bounds a session-init response read. Init answers ids,
	// never bytes.
	responseReadLimit int64 = 1 << 20
)

// Library pushes bytes for one cluster, as the signed-in user.
type Library struct {
	baseURL string
	client  *http.Client
	bearer  func(context.Context) (string, error)
	// oneShotLimit is where this client switches routes. A FIELD rather than
	// the constant read directly, so a test can prove the switch happens
	// without writing 32 MiB to a temp directory -- and so the route a file
	// takes is a property of this client rather than of the package.
	oneShotLimit int64
}

func NewLibrary(baseURL string, client *http.Client, bearer func(context.Context) (string, error)) *Library {
	if client == nil {
		client = http.DefaultClient
	}
	return &Library{
		baseURL:      strings.TrimRight(baseURL, "/"),
		client:       client,
		bearer:       bearer,
		oneShotLimit: OneShotLimitBytes,
	}
}

// PushResult is what the engine answers once a file has landed.
type PushResult struct {
	ArtifactID    string `json:"artifactId"`
	FileID        string `json:"fileId"`
	VersionNumber int    `json:"versionNumber"`
}

// Push sends one file, choosing the route by size.
func (l *Library) Push(ctx context.Context, workerID, path, folderID string, size int64) (PushResult, error) {
	if size <= l.oneShotLimit {
		return l.pushOneShot(ctx, workerID, path, folderID)
	}
	return l.pushSession(ctx, workerID, path, folderID, size)
}

func (l *Library) authorized(ctx context.Context, req *http.Request) error {
	token, err := l.bearer(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// statusError turns a non-2xx into a sentence worth logging and reporting.
//
// 401 and 507 are named because their repairs are specific and a person can
// act on both; everything else carries the server's own body, which is where
// the engine puts the numbers (staged bytes vs declared size, the cap that was
// exceeded, which machine was not one of yours).
func statusError(what string, resp *http.Response, body []byte) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("backup: %s: the cluster rejected this machine's credential (401). Run `memql login` here; a PAT alone cannot reach the Library surface", what)
	case http.StatusInsufficientStorage:
		return fmt.Errorf("backup: %s: this account is out of Library storage (507): %s", what, strings.TrimSpace(string(body)))
	default:
		return fmt.Errorf("backup: %s: HTTP %d: %s", what, resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

func readCapped(r io.Reader) []byte {
	body, _ := io.ReadAll(io.LimitReader(r, responseReadLimit))
	return body
}

// ---------------------------------------------------------------------------
// one shot
// ---------------------------------------------------------------------------

func (l *Library) pushOneShot(ctx context.Context, workerID, path, folderID string) (PushResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return PushResult{}, err
	}
	defer func() { _ = f.Close() }()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return PushResult{}, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return PushResult{}, err
	}
	fields := map[string]string{
		"name":                   filepath.Base(path),
		"uploadedFromWorkerId":   workerID,
		"uploadedFromPath":       path,
		"uploadedFromWorkerName": "",
	}
	if folderID != "" {
		fields["folderId"] = folderID
	}
	for key, value := range fields {
		// The machine NAME is deliberately not claimed. The engine resolves
		// the fleet's own label from the verified registration and that label
		// wins anyway, so sending a guess would be a claim with no effect and
		// one more thing that could disagree with the Fleet page.
		if key == "uploadedFromWorkerName" {
			continue
		}
		if err := writer.WriteField(key, value); err != nil {
			return PushResult{}, err
		}
	}
	if err := writer.Close(); err != nil {
		return PushResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+"/artifacts", &buf)
	if err != nil {
		return PushResult{}, err
	}
	if err := l.authorized(ctx, req); err != nil {
		return PushResult{}, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := l.client.Do(req)
	if err != nil {
		return PushResult{}, fmt.Errorf("backup: upload %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := readCapped(resp.Body)
	if resp.StatusCode/100 != 2 {
		return PushResult{}, statusError("upload "+filepath.Base(path), resp, body)
	}
	var out PushResult
	if err := json.Unmarshal(body, &out); err != nil {
		return PushResult{}, fmt.Errorf("backup: upload %s: decode response: %w", filepath.Base(path), err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// the session route
// ---------------------------------------------------------------------------

type initResponse struct {
	UploadID  string `json:"uploadId"`
	ChunkSize int64  `json:"chunkSize"`
}

type stagedChunk struct {
	N    int   `json:"n"`
	Size int64 `json:"size"`
}

type inventoryResponse struct {
	UploadID  string        `json:"uploadId"`
	Status    string        `json:"status"`
	Size      int64         `json:"size"`
	ChunkSize int64         `json:"chunkSize"`
	Staged    []stagedChunk `json:"staged"`
}

// pushSession opens a session, streams what is missing, and commits.
//
// RESUME IS FREE AND IS USED. The inventory read says which chunks the server
// already has, and a re-PUT of a chunk simply replaces it -- so a session
// interrupted after two of forty chunks resends thirty-eight, not forty.
// On a folder of video over a domestic uplink that is the difference between
// a backup that finishes and one that starts again every time the laptop
// sleeps.
func (l *Library) pushSession(ctx context.Context, workerID, path, folderID string, size int64) (PushResult, error) {
	name := filepath.Base(path)
	body := map[string]any{
		"name":                 name,
		"size":                 size,
		"uploadedFromWorkerId": workerID,
		"uploadedFromPath":     path,
	}
	if folderID != "" {
		body["folderId"] = folderID
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return PushResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+"/artifacts/uploads", bytes.NewReader(payload))
	if err != nil {
		return PushResult{}, err
	}
	if err := l.authorized(ctx, req); err != nil {
		return PushResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.client.Do(req)
	if err != nil {
		return PushResult{}, fmt.Errorf("backup: open session for %s: %w", name, err)
	}
	initBody := readCapped(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return PushResult{}, statusError("open session for "+name, resp, initBody)
	}
	var session initResponse
	if err := json.Unmarshal(initBody, &session); err != nil {
		return PushResult{}, fmt.Errorf("backup: open session for %s: decode response: %w", name, err)
	}
	// THE SERVER'S chunk size, never a local constant. It is recorded on the
	// session row so a session opened under one release completes correctly
	// under another, and a client that assumed its own number would commit a
	// block list the server counts differently.
	chunkSize := session.ChunkSize
	if chunkSize <= 0 {
		return PushResult{}, fmt.Errorf("backup: open session for %s: the cluster named no chunk size", name)
	}

	have, err := l.staged(ctx, session.UploadID)
	if err != nil {
		return PushResult{}, err
	}

	f, err := os.Open(path)
	if err != nil {
		return PushResult{}, err
	}
	defer func() { _ = f.Close() }()

	total := int((size + chunkSize - 1) / chunkSize)
	for n := 1; n <= total; n++ {
		if err := ctx.Err(); err != nil {
			return PushResult{}, err
		}
		if _, ok := have[n]; ok {
			continue
		}
		offset := int64(n-1) * chunkSize
		length := chunkSize
		if remaining := size - offset; remaining < length {
			length = remaining
		}
		if err := l.putChunk(ctx, session.UploadID, n, io.NewSectionReader(f, offset, length), length, name); err != nil {
			return PushResult{}, err
		}
	}
	return l.complete(ctx, session.UploadID, name)
}

func (l *Library) staged(ctx context.Context, uploadID string) (map[int]int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.baseURL+"/artifacts/uploads/"+url.PathEscape(uploadID), nil)
	if err != nil {
		return nil, err
	}
	if err := l.authorized(ctx, req); err != nil {
		return nil, err
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("backup: read staged chunks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := readCapped(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, statusError("read staged chunks", resp, body)
	}
	var inv inventoryResponse
	if err := json.Unmarshal(body, &inv); err != nil {
		return nil, fmt.Errorf("backup: read staged chunks: decode response: %w", err)
	}
	out := make(map[int]int64, len(inv.Staged))
	for _, chunk := range inv.Staged {
		out[chunk.N] = chunk.Size
	}
	return out, nil
}

// putChunk sends one chunk as RAW BYTES -- no multipart, no checksum. `n` is
// 1-based, which is the server's numbering and not an off-by-one here.
func (l *Library) putChunk(ctx context.Context, uploadID string, n int, body io.Reader, length int64, name string) error {
	target := l.baseURL + "/artifacts/uploads/" + url.PathEscape(uploadID) + "/chunks/" + strconv.Itoa(n)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, body)
	if err != nil {
		return err
	}
	if err := l.authorized(ctx, req); err != nil {
		return err
	}
	req.ContentLength = length
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("backup: send chunk %d of %s: %w", n, name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload := readCapped(resp.Body)
	if resp.StatusCode/100 != 2 {
		return statusError(fmt.Sprintf("send chunk %d of %s", n, name), resp, payload)
	}
	return nil
}

func (l *Library) complete(ctx context.Context, uploadID, name string) (PushResult, error) {
	target := l.baseURL + "/artifacts/uploads/" + url.PathEscape(uploadID) + "/complete"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return PushResult{}, err
	}
	if err := l.authorized(ctx, req); err != nil {
		return PushResult{}, err
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return PushResult{}, fmt.Errorf("backup: complete %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := readCapped(resp.Body)
	if resp.StatusCode/100 != 2 {
		// A 409 here means the staged bytes did not add up to what was
		// declared, and the SESSION STAYS OPEN -- so the next sweep re-reads
		// the inventory and sends what is missing rather than starting over.
		// The server's sentence carries the numbers.
		return PushResult{}, statusError("complete "+name, resp, body)
	}
	var out PushResult
	if err := json.Unmarshal(body, &out); err != nil {
		return PushResult{}, fmt.Errorf("backup: complete %s: decode response: %w", name, err)
	}
	return out, nil
}

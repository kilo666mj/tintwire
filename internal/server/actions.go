package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kilo666mj/tintwire/internal/store"
)

const maxActionResponse = 64 << 10
const actionEncryptionKeySetting = "action_encryption_key"

var errActionEncryptionKeyRequired = errors.New("TINTWIRE_ACTION_KEY is required for action credentials")
var errInvalidActionEncryptionKey = errors.New("action encryption key must be base64-encoded 32 bytes")

var actionNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
var operationKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
var carrierGradeNAT = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

type actionService struct {
	store  *store.Store
	seed   string
	key    string
	cipher cipher.AEAD
	mu     sync.RWMutex
}

type actionTargetRequest struct {
	URL          string `json:"url"`
	BearerToken  string `json:"bearer_token"`
	AllowPrivate bool   `json:"allow_private"`
}

type storedHTTPAction struct {
	Label         string `json:"label"`
	Type          string `json:"type"`
	Target        string `json:"target"`
	ContextCipher string `json:"context_cipher"`
}
type storedMattermostAction struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Integration struct {
		Target        string `json:"target"`
		ContextCipher string `json:"context_cipher"`
	} `json:"integration"`
}

func newActionService(data *store.Store, key string) (*actionService, error) {
	service := &actionService{store: data, seed: strings.TrimSpace(key)}
	if _, _, err := service.cipherForStore(context.Background(), data); err != nil {
		if !errors.Is(err, errActionEncryptionKeyRequired) && !errors.Is(err, errInvalidActionEncryptionKey) {
			return nil, err
		}
	}
	return service, nil
}

func (a *actionService) actionKey(ctx context.Context, data *store.Store) (string, error) {
	if data == nil {
		data = a.store
	}
	value, ok, err := data.Setting(ctx, actionEncryptionKeySetting)
	if err != nil {
		return "", err
	}
	if ok {
		value = strings.TrimSpace(value)
		if value != "" {
			return value, nil
		}
	}
	return "", errActionEncryptionKeyRequired
}

func (a *actionService) cipherForStore(ctx context.Context, data *store.Store) (cipher.AEAD, string, error) {
	key, err := a.actionKey(ctx, data)
	if err != nil {
		return nil, "", err
	}
	cipherInstance, err := a.cipherForKey(key)
	return cipherInstance, key, err
}

func (a *actionService) cipherForKey(key string) (cipher.AEAD, error) {
	a.mu.RLock()
	cachedKey := a.key
	cachedCipher := a.cipher
	a.mu.RUnlock()
	if cachedKey == key && cachedCipher != nil {
		return cachedCipher, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(key)
	}
	if err != nil || len(decoded) != 32 {
		return nil, errInvalidActionEncryptionKey
	}
	block, err := aes.NewCipher(decoded)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.key = key
	a.cipher = aead
	a.mu.Unlock()
	return aead, nil
}

func (a *actionService) ensureStoredKey(ctx context.Context, data *store.Store) error {
	if data == nil {
		data = a.store
	}
	existing, ok, err := data.Setting(ctx, actionEncryptionKeySetting)
	if err != nil {
		return err
	}
	if ok && strings.TrimSpace(existing) != "" {
		_, err := a.cipherForKey(strings.TrimSpace(existing))
		return err
	}
	if a.seed == "" {
		return errActionEncryptionKeyRequired
	}
	if _, err := a.cipherForKey(a.seed); err != nil {
		return err
	}
	return data.SaveSettings(ctx, map[string]string{actionEncryptionKeySetting: a.seed})
}

func (a *actionService) encrypt(value string) ([]byte, error) {
	return a.encryptForStore(context.Background(), a.store, value)
}

func (a *actionService) encryptForStore(ctx context.Context, data *store.Store, value string) ([]byte, error) {
	if value == "" {
		return []byte{}, nil
	}
	if a == nil {
		return nil, errors.New("TINTWIRE_ACTION_KEY is required for target credentials")
	}
	cipherInstance, _, err := a.cipherForStore(ctx, data)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, cipherInstance.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return cipherInstance.Seal(nonce, nonce, []byte(value), nil), nil
}
func (a *actionService) decrypt(value []byte) (string, error) {
	if len(value) == 0 {
		return "", nil
	}
	if a == nil {
		return "", errors.New("TINTWIRE_ACTION_KEY is required for action credentials")
	}
	cipherInstance, _, err := a.cipherForStore(context.Background(), a.store)
	if err != nil {
		return "", err
	}
	if len(value) < cipherInstance.NonceSize() {
		return "", errors.New("action credentials are unavailable")
	}
	plain, err := cipherInstance.Open(nil, value[:cipherInstance.NonceSize()], value[cipherInstance.NonceSize():], nil)
	return string(plain), err
}

func (s *Server) protectNativeActionContexts(card nativeCard) (json.RawMessage, error) {
	for index := range card.Actions {
		if card.Actions[index].Type != "http" {
			continue
		}
		contextValue := card.Actions[index].Context
		if len(contextValue) == 0 {
			contextValue = json.RawMessage(`{}`)
		}
		ciphertext, err := s.actions.encrypt(string(contextValue))
		if err != nil {
			return nil, err
		}
		card.Actions[index].Context = nil
		card.Actions[index].ContextCipher = base64.RawStdEncoding.EncodeToString(ciphertext)
	}
	return json.Marshal(card)
}

func (s *Server) protectMattermostActionProps(ctx context.Context, raw json.RawMessage) (json.RawMessage, json.RawMessage, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var props map[string]any
	if json.Unmarshal(raw, &props) != nil {
		return nil, nil, errors.New("invalid post props")
	}
	attachments, ok := props["attachments"].([]any)
	if !ok {
		return raw, nil, nil
	}
	for _, attachmentValue := range attachments {
		attachment, ok := attachmentValue.(map[string]any)
		if !ok {
			continue
		}
		actions, ok := attachment["actions"].([]any)
		if !ok {
			continue
		}
		for _, actionValue := range actions {
			action, ok := actionValue.(map[string]any)
			if !ok {
				continue
			}
			integration, ok := action["integration"].(map[string]any)
			if !ok {
				continue
			}
			targetURL, _ := integration["url"].(string)
			target, err := s.store.ActionTargetByURL(ctx, targetURL)
			if err != nil {
				return nil, nil, errors.New("interactive callback URL is not registered")
			}
			contextValue := integration["context"]
			if contextValue == nil {
				contextValue = map[string]any{}
			}
			plain, err := json.Marshal(contextValue)
			if err != nil {
				return nil, nil, errors.New("invalid interactive callback context")
			}
			ciphertext, err := s.actions.encrypt(string(plain))
			if err != nil {
				return nil, nil, err
			}
			action["integration"] = map[string]any{"target": target.Name, "context_cipher": base64.RawStdEncoding.EncodeToString(ciphertext)}
		}
	}
	protectedProps, err := json.Marshal(props)
	if err != nil {
		return nil, nil, err
	}
	protectedAttachments, err := json.Marshal(attachments)
	return protectedProps, protectedAttachments, err
}

func (s *Server) saveActionTarget(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok || !actor.IsAdmin {
		http.Error(w, "installation administrator access is required", http.StatusForbidden)
		return
	}
	name := r.PathValue("name")
	if !actionNamePattern.MatchString(name) {
		http.Error(w, "invalid action target name", http.StatusBadRequest)
		return
	}
	var request actionTargetRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid action target", http.StatusBadRequest)
		return
	}
	if err := validateActionTargetURL(request.URL, request.AllowPrivate); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		if err := s.actions.ensureStoredKey(r.Context(), data); err != nil {
			return nil, err
		}
		ciphertext, err := s.actions.encryptForStore(r.Context(), data, request.BearerToken)
		if err != nil {
			return nil, err
		}
		return data.SaveActionTarget(r.Context(), name, request.URL, ciphertext, request.AllowPrivate)
	})
	if err != nil {
		if errors.Is(err, errActionEncryptionKeyRequired) || errors.Is(err, errInvalidActionEncryptionKey) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "unable to save action target", http.StatusInternalServerError)
		return
	}
	target := result.(store.ActionTarget)
	writeJSON(w, http.StatusOK, map[string]any{"target": map[string]any{"id": target.ID, "name": target.Name, "url": target.URL, "allow_private": target.AllowPrivate, "has_credentials": len(target.AuthCipher) > 0}})
}

func (s *Server) deleteActionTarget(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok || !actor.IsAdmin {
		http.Error(w, "installation administrator access is required", http.StatusForbidden)
		return
	}
	name := r.PathValue("name")
	if !actionNamePattern.MatchString(name) {
		http.Error(w, "invalid action target name", http.StatusBadRequest)
		return
	}
	_, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return nil, data.DeleteActionTarget(r.Context(), name)
	})
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "action target not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "unable to delete action target", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validateActionTargetURL(value string, allowPrivate bool) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return errors.New("action target must be an absolute URL without user information")
	}
	if parsed.Scheme != "https" && !(allowPrivate && parsed.Scheme == "http") {
		return errors.New("action target must use HTTPS unless private targets are explicitly allowed")
	}
	host := parsed.Hostname()
	if ip := net.ParseIP(host); ip != nil && !allowPrivate && unsafeActionIP(ip) {
		return errors.New("private action target requires allow_private")
	}
	return nil
}

func unsafeActionIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || carrierGradeNAT.Contains(ip)
}

func actionHTTPClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return actionHTTPClientWithNetwork(allowPrivate, net.DefaultResolver.LookupIPAddr, dialer.DialContext)
}

func actionHTTPClientWithNetwork(allowPrivate bool, lookup func(context.Context, string) ([]net.IPAddr, error), dial func(context.Context, string, string) (net.Conn, error)) *http.Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := lookup(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, candidate := range ips {
			if allowPrivate || !unsafeActionIP(candidate.IP) {
				return dial(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			}
		}
		return nil, errors.New("action target resolved only to blocked addresses")
	}}
	return &http.Client{Timeout: 10 * time.Second, Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

func (s *Server) executeHTTPAction(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	operationKey := r.Header.Get("Idempotency-Key")
	if !operationKeyPattern.MatchString(operationKey) {
		http.Error(w, "a valid Idempotency-Key is required", http.StatusBadRequest)
		return
	}
	index, err := parseBoundedIndex(r.PathValue("index"))
	if err != nil {
		http.Error(w, "invalid action index", http.StatusBadRequest)
		return
	}
	card, _, err := s.store.NotificationCardForAction(r.Context(), r.PathValue("id"), actor)
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "notification not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, store.ErrForbidden) {
		http.Error(w, "operator access is required", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "unable to load action", http.StatusInternalServerError)
		return
	}
	var value struct {
		Actions []storedHTTPAction `json:"actions"`
	}
	if json.Unmarshal(card, &value) != nil || index >= len(value.Actions) || value.Actions[index].Type != "http" {
		http.Error(w, "HTTP action not found", http.StatusNotFound)
		return
	}
	action := value.Actions[index]
	execution, fresh, err := s.store.ReserveActionExecution(r.Context(), store.ActionExecution{Key: operationKey, NotificationID: r.PathValue("id"), ActionIndex: index, UserID: actor.ID})
	if errors.Is(err, store.ErrImportConflict) {
		http.Error(w, "idempotency key was used for another operation", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "unable to reserve action", http.StatusInternalServerError)
		return
	}
	if !fresh {
		writeJSON(w, http.StatusOK, map[string]string{"status": execution.Status, "response": execution.ResponseText})
		return
	}
	target, err := s.store.ActionTargetByName(r.Context(), action.Target)
	if err != nil {
		s.finishAction(w, r, operationKey, actor, "failed", "Action target is not registered", http.StatusBadGateway)
		return
	}
	if err := validateActionTargetURL(target.URL, target.AllowPrivate); err != nil {
		s.finishAction(w, r, operationKey, actor, "failed", "Action target is blocked", http.StatusBadGateway)
		return
	}
	token, err := s.actions.decrypt(target.AuthCipher)
	if err != nil {
		s.finishAction(w, r, operationKey, actor, "failed", "Action credentials are unavailable", http.StatusServiceUnavailable)
		return
	}
	contextCipher, err := base64.RawStdEncoding.DecodeString(action.ContextCipher)
	if err != nil {
		s.finishAction(w, r, operationKey, actor, "failed", "Action context is unavailable", http.StatusServiceUnavailable)
		return
	}
	contextText, err := s.actions.decrypt(contextCipher)
	if err != nil {
		s.finishAction(w, r, operationKey, actor, "failed", "Action context is unavailable", http.StatusServiceUnavailable)
		return
	}
	payload, _ := json.Marshal(map[string]any{"notification_id": r.PathValue("id"), "action": action.Label, "actor": map[string]string{"id": actor.ID, "username": actor.Username}, "context": json.RawMessage(contextText), "operation_key": operationKey})
	callback, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.URL, strings.NewReader(string(payload)))
	if err != nil {
		s.finishAction(w, r, operationKey, actor, "failed", "Action request could not be created", http.StatusBadGateway)
		return
	}
	callback.Header.Set("Content-Type", "application/json")
	callback.Header.Set("Idempotency-Key", operationKey)
	if token != "" {
		callback.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := actionHTTPClient(target.AllowPrivate).Do(callback)
	if err != nil {
		s.finishAction(w, r, operationKey, actor, "failed", "Action target could not be reached", http.StatusBadGateway)
		return
	}
	defer func() { _ = response.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxActionResponse+1))
	if readErr != nil || len(body) > maxActionResponse {
		s.finishAction(w, r, operationKey, actor, "failed", "Action response was too large", http.StatusBadGateway)
		return
	}
	text := strings.TrimSpace(string(body))
	var jsonResponse struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(body, &jsonResponse) == nil && strings.TrimSpace(jsonResponse.Text) != "" {
		text = strings.TrimSpace(jsonResponse.Text)
	}
	if len(text) > 1000 {
		text = text[:1000]
	}
	if text == "" {
		text = http.StatusText(response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.finishAction(w, r, operationKey, actor, "failed", fmt.Sprintf("Action failed: %s", text), http.StatusBadGateway)
		return
	}
	s.finishAction(w, r, operationKey, actor, "succeeded", text, http.StatusOK)
}

func (s *Server) executeMattermostAction(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	operationKey := r.Header.Get("Idempotency-Key")
	if !operationKeyPattern.MatchString(operationKey) {
		http.Error(w, "a valid Idempotency-Key is required", http.StatusBadRequest)
		return
	}
	attachmentIndex, err := parseBoundedIndex(r.PathValue("attachment"))
	if err != nil {
		http.Error(w, "invalid attachment index", http.StatusBadRequest)
		return
	}
	actionIndex, err := parseBoundedIndex(r.PathValue("action"))
	if err != nil {
		http.Error(w, "invalid action index", http.StatusBadRequest)
		return
	}
	source, err := s.store.MattermostActionForNotification(r.Context(), r.PathValue("id"), actor)
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "notification not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, store.ErrForbidden) {
		http.Error(w, "operator access is required", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "unable to load action", http.StatusInternalServerError)
		return
	}
	var attachments []struct {
		Actions []storedMattermostAction `json:"actions"`
	}
	if json.Unmarshal(source.Attachments, &attachments) != nil || attachmentIndex >= len(attachments) || actionIndex >= len(attachments[attachmentIndex].Actions) {
		http.Error(w, "interactive action not found", http.StatusNotFound)
		return
	}
	action := attachments[attachmentIndex].Actions[actionIndex]
	if action.Integration.Target == "" || action.Integration.ContextCipher == "" {
		http.Error(w, "interactive action not found", http.StatusNotFound)
		return
	}
	completed, err := s.store.LatestMattermostActionResults(r.Context(), []string{r.PathValue("id")})
	if err != nil {
		http.Error(w, "unable to load action state", http.StatusInternalServerError)
		return
	}
	if result, ok := completed[r.PathValue("id")][attachmentIndex]; ok && result.Status == "succeeded" {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": result.Status, "response": result.ResponseText, "actor": result.Actor,
			"action_index": result.ActionIndex, "completed_at": result.CompletedAt,
		})
		return
	}
	combinedIndex := 10000 + attachmentIndex*101 + actionIndex
	execution, fresh, err := s.store.ReserveActionExecution(r.Context(), store.ActionExecution{Key: operationKey, NotificationID: r.PathValue("id"), ActionIndex: combinedIndex, UserID: actor.ID})
	if errors.Is(err, store.ErrImportConflict) {
		http.Error(w, "idempotency key was used for another operation", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "unable to reserve action", http.StatusInternalServerError)
		return
	}
	if !fresh {
		writeJSON(w, http.StatusOK, map[string]string{"status": execution.Status, "response": execution.ResponseText})
		return
	}
	target, err := s.store.ActionTargetByName(r.Context(), action.Integration.Target)
	if err != nil {
		s.finishAction(w, r, operationKey, actor, "failed", "Action target is not registered", http.StatusBadGateway)
		return
	}
	if err := validateActionTargetURL(target.URL, target.AllowPrivate); err != nil {
		s.finishAction(w, r, operationKey, actor, "failed", "Action target is blocked", http.StatusBadGateway)
		return
	}
	token, err := s.actions.decrypt(target.AuthCipher)
	if err != nil {
		s.finishAction(w, r, operationKey, actor, "failed", "Action credentials are unavailable", http.StatusServiceUnavailable)
		return
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(action.Integration.ContextCipher)
	if err != nil {
		s.finishAction(w, r, operationKey, actor, "failed", "Action context is unavailable", http.StatusServiceUnavailable)
		return
	}
	contextText, err := s.actions.decrypt(ciphertext)
	if err != nil {
		s.finishAction(w, r, operationKey, actor, "failed", "Action context is unavailable", http.StatusServiceUnavailable)
		return
	}
	payload, _ := json.Marshal(map[string]any{"user_id": actor.ID, "user_name": actor.Username, "post_id": source.PostID, "channel_id": source.ChannelID, "context": json.RawMessage(contextText), "action": action.ID})
	callback, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.URL, strings.NewReader(string(payload)))
	if err != nil {
		s.finishAction(w, r, operationKey, actor, "failed", "Action request could not be created", http.StatusBadGateway)
		return
	}
	callback.Header.Set("Content-Type", "application/json")
	callback.Header.Set("Idempotency-Key", operationKey)
	if token != "" {
		callback.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := actionHTTPClient(target.AllowPrivate).Do(callback)
	if err != nil {
		s.finishAction(w, r, operationKey, actor, "failed", "Action target could not be reached", http.StatusBadGateway)
		return
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxActionResponse+1))
	if err != nil || len(body) > maxActionResponse {
		s.finishAction(w, r, operationKey, actor, "failed", "Action response was too large", http.StatusBadGateway)
		return
	}
	text := strings.TrimSpace(string(body))
	var compatible struct {
		EphemeralText string `json:"ephemeral_text"`
		Text          string `json:"text"`
		Update        *struct {
			Message string `json:"message"`
		} `json:"update"`
	}
	if json.Unmarshal(body, &compatible) == nil {
		if compatible.EphemeralText != "" {
			text = compatible.EphemeralText
		} else if compatible.Text != "" {
			text = compatible.Text
		} else if compatible.Update != nil && compatible.Update.Message != "" {
			text = compatible.Update.Message
		}
	}
	if len(text) > 1000 {
		text = text[:1000]
	}
	if text == "" {
		text = http.StatusText(response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.finishAction(w, r, operationKey, actor, "failed", "Action failed: "+text, http.StatusBadGateway)
		return
	}
	s.finishAction(w, r, operationKey, actor, "succeeded", text, http.StatusOK)
}

func parseBoundedIndex(value string) (int, error) {
	index, err := strconv.Atoi(value)
	if err != nil || index < 0 || index > 100 {
		return 0, errors.New("bad index")
	}
	return index, nil
}

func (s *Server) finishAction(w http.ResponseWriter, r *http.Request, key string, actor store.User, status, text string, httpStatus int) {
	if err := s.store.CompleteActionExecution(r.Context(), key, status, text, actor); err != nil {
		http.Error(w, "unable to record action result", http.StatusInternalServerError)
		return
	}
	s.publish(r.PathValue("id"))
	writeJSON(w, httpStatus, map[string]string{"status": status, "response": text})
}

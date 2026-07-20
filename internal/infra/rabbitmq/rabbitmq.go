package rabbitmq

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"jiuxiaoer-admin/backend-go/internal/config"
)

// Manager owns a reconnectable RabbitMQ connection. Workers ask for the
// current connection for every session instead of retaining a dead pointer.
type Manager struct {
	cfg    config.RabbitMQConfig
	log    *slog.Logger
	mu     sync.Mutex
	conn   *amqp.Connection
	closed bool
}

// Open 解密并返回Manager。
func Open(ctx context.Context, cfg config.RabbitMQConfig, log *slog.Logger) (*Manager, error) {
	if cfg.URL == "" {
		if cfg.Required {
			return nil, fmt.Errorf("JXE_RABBITMQ_URL is required")
		}
		log.Warn("rabbitmq disabled because JXE_RABBITMQ_URL is empty")
		return nil, nil
	}

	manager := &Manager{cfg: cfg, log: log}
	if _, err := manager.connectOnce(ctx); err != nil {
		if cfg.Required {
			return nil, err
		}
		log.Warn("rabbitmq initial connection failed; workers will retry", slog.Any("error", err))
	}
	return manager, nil
}

// Connection 返回连接。
func (m *Manager) Connection(ctx context.Context) (*amqp.Connection, error) {
	if m == nil {
		return nil, fmt.Errorf("rabbitmq is disabled")
	}
	backoff := m.cfg.ReconnectMin
	for {
		conn, err := m.connectOnce(ctx)
		if err == nil {
			return conn, nil
		}
		m.log.Warn("rabbitmq reconnect failed", slog.Duration("retry_in", backoff), slog.Any("error", err))
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		backoff *= 2
		if backoff > m.cfg.ReconnectMax {
			backoff = m.cfg.ReconnectMax
		}
	}
}

// connectOnce 返回connect Once。
func (m *Manager) connectOnce(ctx context.Context) (*amqp.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("rabbitmq manager is closed")
	}
	if m.conn != nil && !m.conn.IsClosed() {
		return m.conn, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	dialer := net.Dialer{Timeout: m.cfg.DialTimeout}
	conn, err := amqp.DialConfig(m.cfg.URL, amqp.Config{
		Dial: func(network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
	})
	if err != nil {
		return nil, err
	}
	m.conn = conn
	m.log.Info("rabbitmq connected")
	return conn, nil
}

// Healthy 判断Healthy。
func (m *Manager) Healthy() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.closed && m.conn != nil && !m.conn.IsClosed()
}

// ManagementGet 返回Management Get。
// ManagementGet reads RabbitMQ Management API state without mutating broker
// resources. It is used for exact topology verification and queue statistics;
// AMQP passive declarations alone cannot expose exchange/queue arguments.
func (m *Manager) ManagementGet(ctx context.Context, resource string, destination any) error {
	if m == nil {
		return fmt.Errorf("rabbitmq is disabled")
	}
	endpoint, username, password, err := managementEndpoint(m.cfg, resource)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if username != "" {
		request.SetBasicAuth(username, password)
	}
	client := &http.Client{Timeout: m.cfg.DialTimeout}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("rabbitmq management %s returned %s", resource, response.Status)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 8<<20))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode rabbitmq management %s: %w", resource, err)
	}
	return nil
}

// managementEndpoint 返回management Endpoint。
func managementEndpoint(cfg config.RabbitMQConfig, resource string) (string, string, string, error) {
	amqpURL, err := url.Parse(cfg.URL)
	if err != nil || amqpURL.Hostname() == "" {
		return "", "", "", fmt.Errorf("invalid RabbitMQ URL")
	}
	baseRaw := cfg.ManagementURL
	if baseRaw == "" {
		scheme, port := "http", "15672"
		if amqpURL.Scheme == "amqps" {
			scheme, port = "https", "15671"
		}
		baseRaw = (&url.URL{Scheme: scheme, Host: net.JoinHostPort(amqpURL.Hostname(), port), Path: "/api"}).String()
	}
	base, err := url.Parse(baseRaw)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", "", "", fmt.Errorf("invalid RabbitMQ management URL")
	}
	username, password := "", ""
	credentials := base.User
	if credentials == nil {
		credentials = amqpURL.User
	}
	if credentials != nil {
		username = credentials.Username()
		password, _ = credentials.Password()
	}
	base.User = nil
	vhost := "/"
	if escapedPath := strings.TrimPrefix(amqpURL.EscapedPath(), "/"); escapedPath != "" {
		if decoded, decodeErr := url.PathUnescape(escapedPath); decodeErr == nil && decoded != "" {
			vhost = decoded
		}
	}
	endpoint := strings.TrimRight(base.String(), "/") + "/" + strings.Trim(resource, "/") + "/" + url.PathEscape(vhost)
	return endpoint, username, password, nil
}

// Close 关闭当前实例并释放相关资源。
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	if m.conn != nil && !m.conn.IsClosed() {
		return m.conn.Close()
	}
	return nil
}

package egress

import (
	"net"
	"net/http"
	"sync"
)

// ClientCache 按出口 IP 缓存 http.Client。
//
// 为什么要按 IP 缓存：http.Transport 的 Keep-Alive 连接池与「源地址」绑定，
// 不同出口共用一个 Transport 会把连接混用（transport 会复用同一 host 的连接，
// 而那条连接是从别的源 IP 建立的）⇒ 出口隔离名存实亡。所以每个出口一个
// Transport，各自独立连接池。
type ClientCache struct {
	mu    sync.Mutex
	build func(net.IP) *http.Client
	m     map[string]*http.Client
}

// NewClientCache 构造缓存；build 由调用方提供（upstream / account 的
// Transport 参数不同：超时、HTTP/2、空闲连接数）。
func NewClientCache(build func(net.IP) *http.Client) *ClientCache {
	return &ClientCache{build: build, m: map[string]*http.Client{}}
}

// Get 取该出口的 client；ip 为 nil 时返回直连 client。
func (c *ClientCache) Get(ip net.IP) *http.Client {
	if c == nil {
		return nil
	}
	key := "direct"
	if ip != nil {
		key = ip.String()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.m[key]; ok {
		return cl
	}
	// 地址数量就是本机出口数（几十个量级），不需要 LRU；真超了就整体重建，
	// 让旧 Transport 被回收，避免长期运行后 map 无界增长。
	if len(c.m) > 256 {
		c.m = map[string]*http.Client{}
	}
	cl := c.build(ip)
	c.m[key] = cl
	return cl
}

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
)

var (
	db          *sql.DB
	rdb         *redis.Client
	kafkaWriter *kafka.Writer
	instanceID  = os.Getenv("INSTANCE_ID")
	ctx         = context.Background()
	cacheTTL    = 30 * time.Second

	reqCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "product_requests_total",
		Help: "Total requests served",
	}, []string{"instance", "status", "cache"})

	reqDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "product_request_duration_seconds",
		Help:    "Request latency",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1},
	}, []string{"instance"})

	dbQueryCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "product_db_queries_total",
		Help: "DB queries (cache misses)",
	})

	cacheHitCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "product_cache_total",
		Help: "Cache HIT/MISS counter",
	}, []string{"result"})

	orderCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "product_orders_total",
		Help: "Order request counter",
	}, []string{"path", "status"}) // path=async/sync, status=accepted/failed
)

type Product struct {
	ID    int     `json:"id"`
	Name  string  `json:"name"`
	Price float64 `json:"price"`
	Stock int     `json:"stock"`
}

func getProduct(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		reqCount.WithLabelValues(instanceID, "400", "-").Inc()
		return
	}
	cacheKey := "product:" + id
	w.Header().Set("X-Instance", instanceID)

	defer func() {
		reqDuration.WithLabelValues(instanceID).Observe(time.Since(start).Seconds())
	}()

	if v, err := rdb.Get(ctx, cacheKey).Result(); err == nil {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(v))
		cacheHitCount.WithLabelValues("hit").Inc()
		reqCount.WithLabelValues(instanceID, "200", "hit").Inc()
		return
	}

	var p Product
	err := db.QueryRow("SELECT id, name, price, stock FROM products WHERE id=?", id).
		Scan(&p.ID, &p.Name, &p.Price, &p.Stock)
	dbQueryCount.Inc()
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		reqCount.WithLabelValues(instanceID, "404", "miss").Inc()
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		reqCount.WithLabelValues(instanceID, "500", "miss").Inc()
		return
	}

	body, _ := json.Marshal(p)
	rdb.Set(ctx, cacheKey, body, cacheTTL)

	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
	cacheHitCount.WithLabelValues("miss").Inc()
	reqCount.WithLabelValues(instanceID, "200", "miss").Inc()
}

type OrderReq struct {
	UserID    int `json:"user_id"`
	ProductID int `json:"product_id"`
	Quantity  int `json:"quantity"`
}

// 异步：写 Kafka，立刻 202 返回
func postOrderAsync(w http.ResponseWriter, r *http.Request) {
	var req OrderReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		orderCount.WithLabelValues("async", "failed").Inc()
		return
	}
	body, _ := json.Marshal(req)
	// key=user_id：同一用户的所有订单进同一 partition（保序+局部并发）
	key := []byte(strconv.Itoa(req.UserID))
	if err := kafkaWriter.WriteMessages(r.Context(), kafka.Message{Key: key, Value: body}); err != nil {
		http.Error(w, "kafka write failed: "+err.Error(), http.StatusServiceUnavailable)
		orderCount.WithLabelValues("async", "failed").Inc()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(`{"status":"accepted","path":"async"}`))
	orderCount.WithLabelValues("async", "accepted").Inc()
}

// 同步：直接写 MySQL，等返回。对照组，用来感受"同步打挂"的场景
func postOrderSync(w http.ResponseWriter, r *http.Request) {
	var req OrderReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		orderCount.WithLabelValues("sync", "failed").Inc()
		return
	}
	_, err := db.ExecContext(r.Context(),
		"INSERT INTO orders(user_id, product_id, quantity) VALUES(?,?,?)",
		req.UserID, req.ProductID, req.Quantity)
	if err != nil {
		http.Error(w, "db insert failed: "+err.Error(), http.StatusServiceUnavailable)
		orderCount.WithLabelValues("sync", "failed").Inc()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok","path":"sync"}`))
	orderCount.WithLabelValues("sync", "accepted").Inc()
}

func health(w http.ResponseWriter, r *http.Request) {
	if err := db.Ping(); err != nil {
		http.Error(w, "db down", 503)
		return
	}
	if err := rdb.Ping(ctx).Err(); err != nil {
		http.Error(w, "redis down", 503)
		return
	}
	fmt.Fprintf(w, "ok from %s", instanceID)
}

func main() {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		dsn = "root:root@tcp(mysql:3306)/shop?parseTime=true"
	}
	var err error
	for i := 0; i < 30; i++ {
		db, err = sql.Open("mysql", dsn)
		if err == nil {
			err = db.Ping()
		}
		if err == nil {
			break
		}
		log.Printf("waiting for mysql... %v", err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Fatal("mysql connect failed: ", err)
	}
	maxConns, _ := strconv.Atoi(os.Getenv("DB_MAX_CONNS"))
	if maxConns == 0 {
		maxConns = 50
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(10)

	sentinelAddrs := os.Getenv("REDIS_SENTINEL_ADDRS")
	if sentinelAddrs != "" {
		// 走 Sentinel 发现 master，主从切换时自动重连新 master
		rdb = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:    os.Getenv("REDIS_MASTER_NAME"),
			SentinelAddrs: strings.Split(sentinelAddrs, ","),
		})
		log.Printf("redis: using sentinel discovery, master=%s sentinels=%s",
			os.Getenv("REDIS_MASTER_NAME"), sentinelAddrs)
	} else {
		// 直连模式（阶段 2.1 之前的兼容方式）
		rdb = redis.NewClient(&redis.Options{
			Addr: os.Getenv("REDIS_ADDR"),
		})
		log.Printf("redis: direct connect %s", os.Getenv("REDIS_ADDR"))
	}
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatal("redis connect failed: ", err)
	}

	kafkaBroker := os.Getenv("KAFKA_BROKER")
	if kafkaBroker != "" {
		// 支持逗号分隔的多 broker bootstrap list
		brokers := strings.Split(kafkaBroker, ",")
		kafkaWriter = &kafka.Writer{
			Addr:         kafka.TCP(brokers...),
			Topic:        "orders",
			Balancer:     &kafka.Hash{},     // 按 key 哈希到 partition（保证同 user 的订单进同分区）
			RequiredAcks: kafka.RequireAll,  // ★ acks=all：等所有 ISR 写完才算成功，最强一致
			BatchTimeout: 10 * time.Millisecond,
			Async:        false,
		}
		log.Printf("kafka writer ready, brokers=%v topic=orders acks=all", brokers)
	} else {
		log.Println("kafka not configured (KAFKA_BROKER empty), /order will fail")
	}

	http.HandleFunc("/product", getProduct)
	http.HandleFunc("/order", postOrderAsync)
	http.HandleFunc("/order/sync", postOrderSync)
	http.HandleFunc("/health", health)
	http.Handle("/metrics", promhttp.Handler())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("instance=%s listening on :%s", instanceID, port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

package main

import (
	"context"
	"database/sql"
	"encoding/json"
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
	dto "github.com/prometheus/client_model/go"
	"github.com/segmentio/kafka-go"
)

func getCounterValue(c prometheus.Counter) float64 {
	m := &dto.Metric{}
	_ = c.Write(m)
	return m.GetCounter().GetValue()
}

type Order struct {
	UserID    int `json:"user_id"`
	ProductID int `json:"product_id"`
	Quantity  int `json:"quantity"`
}

var (
	consumedCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "worker_orders_consumed_total",
		Help: "Orders consumed from Kafka",
	})
	failedCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "worker_orders_failed_total",
		Help: "Orders failed to write to MySQL",
	})
	lagGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "worker_processing_seconds",
		Help: "Time from kafka message to MySQL commit",
	})
)

func main() {
	dsn := os.Getenv("MYSQL_DSN")
	broker := os.Getenv("KAFKA_BROKER")
	rateStr := os.Getenv("RATE_PER_SEC")
	rate, _ := strconv.Atoi(rateStr)
	if rate == 0 {
		rate = 200 // 故意慢消费，演示削峰：1秒处理 200 个
	}

	var db *sql.DB
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

	brokers := strings.Split(broker, ",")
	workerID := os.Getenv("WORKER_ID")
	if workerID == "" {
		workerID = "worker"
	}
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     brokers,
		Topic:       "orders",
		GroupID:     "order-worker",
		StartOffset: kafka.FirstOffset, // 新组从头读，避免错过历史数据
		MinBytes:    1,
		MaxBytes:    10e6,
	})
	defer reader.Close()

	log.Printf("[%s] worker ready: brokers=%v rate=%d msg/s", workerID, brokers, rate)

	// /metrics 端点
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		log.Fatal(http.ListenAndServe(":8090", nil))
	}()

	// 限速：每条消息处理之间至少间隔 1/rate 秒
	throttle := time.NewTicker(time.Second / time.Duration(rate))
	defer throttle.Stop()

	ctx := context.Background()
	for {
		msg, err := reader.ReadMessage(ctx)
		if err != nil {
			log.Printf("read error: %v", err)
			time.Sleep(time.Second)
			continue
		}

		<-throttle.C // 人为节流，模拟慢消费

		start := time.Now()
		var o Order
		if err := json.Unmarshal(msg.Value, &o); err != nil {
			log.Printf("bad message: %v", err)
			failedCount.Inc()
			continue
		}
		_, err = db.Exec(
			"INSERT INTO orders(user_id, product_id, quantity) VALUES(?,?,?)",
			o.UserID, o.ProductID, o.Quantity)
		if err != nil {
			log.Printf("[%s] mysql insert failed: %v", workerID, err)
			failedCount.Inc()
			continue
		}
		consumedCount.Inc()
		lagGauge.Set(time.Since(start).Seconds())
		// 每 200 条打一行日志，证明并行消费
		if consumedTotal := int(getCounterValue(consumedCount)); consumedTotal%200 == 0 {
			log.Printf("[%s] consumed=%d, last msg from partition=%d offset=%d",
				workerID, consumedTotal, msg.Partition, msg.Offset)
		}
	}
}

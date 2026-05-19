package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

// 故意烧 CPU n 毫秒，演示 HPA 触发
func burnCPU(ms int) {
	deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
	x := 0.0001
	for time.Now().Before(deadline) {
		// 浮点乘法，编译器很难优化掉
		for i := 0; i < 100000; i++ {
			x = x * 1.0000001
		}
	}
	_ = x
}

func main() {
	hostname, _ := os.Hostname()
	version := os.Getenv("APP_VERSION")
	if version == "" {
		version = "v1"
	}

	mux := http.NewServeMux()

	// 主页：返回 Pod 信息，方便看分流
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Pod", hostname)
		w.Header().Set("X-Version", version)
		fmt.Fprintf(w, `{"pod":"%s","version":"%s"}`+"\n", hostname, version)
	})

	// 关键：烧 CPU 触发 HPA
	mux.HandleFunc("/load", func(w http.ResponseWriter, r *http.Request) {
		msStr := r.URL.Query().Get("ms")
		ms, _ := strconv.Atoi(msStr)
		if ms == 0 {
			ms = 200
		}
		burnCPU(ms)
		w.Header().Set("X-Pod", hostname)
		fmt.Fprintf(w, `{"burned_ms":%d,"pod":"%s"}`+"\n", ms, hostname)
	})

	// K8s liveness/readiness probe 用
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("listening on :%s, pod=%s version=%s", port, hostname, version)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

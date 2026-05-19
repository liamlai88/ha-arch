-- ============================================
-- 三个内部账号（这些 CREATE USER 会通过 binlog 复制到 slave）
-- ============================================

-- 复制账号：slave 用它拉 binlog
CREATE USER 'repl'@'%' IDENTIFIED WITH mysql_native_password BY 'repl';
GRANT REPLICATION SLAVE ON *.* TO 'repl'@'%';

-- ProxySQL 监控账号：探测主从角色（@@read_only）
CREATE USER 'monitor'@'%' IDENTIFIED WITH mysql_native_password BY 'monitor';
GRANT USAGE, REPLICATION CLIENT ON *.* TO 'monitor'@'%';

-- Prometheus mysqld-exporter 监控账号
CREATE USER 'exporter'@'%' IDENTIFIED WITH mysql_native_password BY 'exporter' WITH MAX_USER_CONNECTIONS 3;
GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.* TO 'exporter'@'%';

FLUSH PRIVILEGES;

-- ============================================
-- 业务库（也通过 binlog 复制到 slave）
-- ============================================
CREATE DATABASE IF NOT EXISTS shop CHARACTER SET utf8mb4;
USE shop;

CREATE TABLE IF NOT EXISTS products (
  id INT PRIMARY KEY AUTO_INCREMENT,
  name VARCHAR(100) NOT NULL,
  price DECIMAL(10,2) NOT NULL,
  stock INT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS orders (
  id BIGINT PRIMARY KEY AUTO_INCREMENT,
  user_id INT NOT NULL,
  product_id INT NOT NULL,
  quantity INT NOT NULL,
  status VARCHAR(20) DEFAULT 'pending',
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  INDEX idx_user (user_id),
  INDEX idx_created (created_at)
);

INSERT INTO products (name, price, stock) VALUES
  ('iPhone 17 Pro',  9999.00, 100),
  ('MacBook Air M5', 11999.00, 50),
  ('AirPods Pro 3',  1999.00,  200),
  ('iPad Pro',       7999.00,  80),
  ('Apple Watch',    3299.00,  150);

-- 等 slave 容器初始化完，建立和 master 的复制连接
-- 使用 GTID auto position，slave 会自动找出"从哪个事务开始拉"
CHANGE REPLICATION SOURCE TO
    SOURCE_HOST = 'mysql-master',
    SOURCE_PORT = 3306,
    SOURCE_USER = 'repl',
    SOURCE_PASSWORD = 'repl',
    SOURCE_AUTO_POSITION = 1,
    GET_SOURCE_PUBLIC_KEY = 1;

START REPLICA;

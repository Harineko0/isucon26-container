package isureal

import (
	"net"
	"net/url"
	"os"
)

type Config struct {
	Region     string
	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	DBSSLMode  string
	LogLevel   string
}

func ConfigFromEnv() *Config {
	return &Config{
		Region:     envOrDefault("ISUREAL_REGION", "asia"),
		DBHost:     envOrDefault("ISUREAL_DB_HOST", "127.0.0.1"),
		DBPort:     envOrDefault("ISUREAL_DB_PORT", "5432"),
		DBUser:     envOrDefault("ISUREAL_DB_USER", "isureal"),
		DBPassword: envOrDefault("ISUREAL_DB_PASSWORD", "isureal"),
		DBName:     envOrDefault("ISUREAL_DB_NAME", "isureal"),
		// 未指定だと pgx は sslmode=prefer で毎回 TLS 交渉する。EU/US では新規接続が
		// ~0.43s → ~0.74s (100ms RTT × 3 往復増) になるので既定は disable。
		// DB の pg_hba は host (hostssl ではない) なので平文接続は許可されている。
		DBSSLMode: envOrDefault("ISUREAL_DB_SSLMODE", "disable"),
		LogLevel:  envOrDefault("ISUREAL_LOG_LEVEL", "debug"),
	}
}

func (c *Config) DatabaseDSN() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.DBUser, c.DBPassword),
		Host:   net.JoinHostPort(c.DBHost, c.DBPort),
		Path:   "/" + c.DBName,
	}
	if c.DBSSLMode != "" {
		u.RawQuery = url.Values{"sslmode": {c.DBSSLMode}}.Encode()
	}
	return u.String()
}

func envOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

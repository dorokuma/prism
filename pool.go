package main

import (
	"github.com/dorokuma/prism/internal/pool"
)

type AccountStatus = pool.AccountStatus

const (
	StatusHealthy   = pool.StatusHealthy
	StatusExhausted = pool.StatusExhausted
)

type Account = pool.Account
type Pool = pool.Pool
type PoolSnapshot = pool.PoolSnapshot

var (
	NewPool              = pool.NewPool
	ErrNoHealthyAccounts = pool.ErrNoHealthyAccounts
	ErrSelectTimeout     = pool.ErrSelectTimeout
)

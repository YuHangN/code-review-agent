package reviewfixture

import (
	"errors"
	"sync"
)

var (
	errForbidden     = errors.New("forbidden")
	errInvalidAmount = errors.New("invalid amount")
	errInsufficient  = errors.New("insufficient balance")
)

type Wallet struct {
	mu      sync.Mutex
	owner   string
	balance int
}

func NewWallet(owner string, balance int) *Wallet {
	return &Wallet{owner: owner, balance: balance}
}

func Snapshot(wallet Wallet) int {
	wallet.mu.Lock()
	defer wallet.mu.Unlock()
	return wallet.balance
}

func Withdraw(wallet *Wallet, actor string, amount int) error {
	if amount <= 0 {
		return errInvalidAmount
	}

	wallet.mu.Lock()
	defer wallet.mu.Unlock()
	if amount > wallet.balance {
		return errInsufficient
	}

	wallet.balance -= amount
	if actor != wallet.owner {
		return errForbidden
	}
	return nil
}

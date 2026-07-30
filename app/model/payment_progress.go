package model

import (
	"sync"

	"github.com/spf13/cast"
)

type PaymentProgress struct {
	Current  int `json:"current"`
	Required int `json:"required"`
}

var chainProgressState sync.Map

func SetChainProgress(network Network, height int) {
	chainProgressState.Store(network, height)
}

func GetChainProgress(order Order) PaymentProgress {
	progress := PaymentProgress{}
	if order.Status != OrderStatusConfirming {
		return progress
	}

	conf, ok := registry[order.TradeType]
	if !ok {
		return progress
	}

	required := getRequiredConfirmations(conf.Network)
	if required <= 0 {
		progress.Required = 1
		progress.Current = 1
		return progress
	}

	progress.Required = required

	height, ok := chainProgressState.Load(conf.Network)
	if !ok {
		return progress
	}

	current := cast.ToInt(height) - order.RefBlockNum
	if current < 0 {
		current = 0
	}
	if current > required {
		current = required
	}

	progress.Current = current
	return progress
}

func getRequiredConfirmations(network Network) int {
	switch network {
	case "tron":
		return 30
	case "bsc":
		return 15
	case "ethereum":
		return 12
	case "polygon":
		return 40
	case "arbitrum":
		return 40
	case "base":
		return 40
	case "xlayer":
		return 12
	case "plasma":
		return 40
	case "solana":
		return 60
	case "aptos":
		return 1000
	default:
		return 0
	}
}

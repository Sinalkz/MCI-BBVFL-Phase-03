package main

import (
	"log"

	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
)

func main() {
	chaincode, err := contractapi.NewChaincode(&SmartContract{})
	if err != nil {
		log.Panicf("error creating VFL audit chaincode: %v", err)
	}

	if err := chaincode.Start(); err != nil {
		log.Panicf("error starting VFL audit chaincode: %v", err)
	}
}

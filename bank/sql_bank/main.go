// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Tamir Duberstein

package main

import (
	"database/sql"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/lib/pq"

	"github.com/cockroachdb/cockroach/security"
	"github.com/cockroachdb/cockroach/security/securitytest"
	"github.com/cockroachdb/cockroach/server"
	"github.com/cockroachdb/cockroach/util/log"
)

var maxTransfer = flag.Int("max-transfer", 999, "Maximum amount to transfer in one transaction.")
var numAccounts = flag.Int("num-accounts", 999, "Number of accounts.")
var concurrency = flag.Int("concurrency", 5, "Number of concurrent actors moving money.")
var transferStyle = flag.String("transfer-style", "txn", "\"single-stmt\" or \"txn\"")
var usePostgres = flag.Bool("use-postgres", false, "Use postgres instead of cockroach.")
var balanceCheckInterval = flag.Duration("balance-check-interval", 1*time.Second, "Interval of balance check.")

type measurement struct {
	read, write, total time.Duration
}

func moveMoney(db *sql.DB, readings chan<- measurement) {
	for {
		from, to := rand.Intn(*numAccounts), rand.Intn(*numAccounts)
		if from == to {
			continue
		}
		amount := rand.Intn(*maxTransfer)
		switch *transferStyle {
		case "single-stmt":
			update := `
UPDATE bank.accounts
  SET balance = CASE id WHEN $1 THEN balance-$3 WHEN $2 THEN balance+$3 END
  WHERE id IN ($1, $2) AND (SELECT balance >= $3 FROM bank.accounts WHERE id = $1)
`
			start := time.Now()
			result, err := db.Exec(update, from, to, amount)
			if err != nil {
				if log.V(1) {
					log.Warning(err)
				}
				continue
			}
			affected, err := result.RowsAffected()
			if err != nil {
				log.Fatal(err)
			}
			if affected > 0 {
				d := time.Since(start)
				readings <- measurement{read: d, write: d, total: d}
			}

		case "txn":
			start := time.Now()
			tx, err := db.Begin()
			if err != nil {
				log.Fatal(err)
			}
			startRead := time.Now()
			rows, err := tx.Query(`SELECT id, balance FROM accounts WHERE id IN ($1, $2)`, from, to)
			if err != nil {
				if log.V(1) {
					log.Warning(err)
				}
				if err = tx.Rollback(); err != nil {
					log.Fatal(err)
				}
				continue
			}
			readDuration := time.Since(startRead)
			var fromBalance, toBalance int
			for rows.Next() {
				var id, balance int
				if err = rows.Scan(&id, &balance); err != nil {
					log.Fatal(err)
				}
				switch id {
				case from:
					fromBalance = balance
				case to:
					toBalance = balance
				default:
					panic(fmt.Sprintf("got unexpected account %d", id))
				}
			}
			startWrite := time.Now()
			if fromBalance >= amount {
				update := `UPDATE bank.accounts
  SET balance = CASE id WHEN $1 THEN $3 WHEN $2 THEN $4 END
  WHERE id IN ($1, $2)`
				if _, err = tx.Exec(update, to, from, toBalance+amount, fromBalance-amount, from); err != nil {
					if log.V(1) {
						log.Warning(err)
					}
					if err = tx.Rollback(); err != nil {
						log.Fatal(err)
					}
					continue
				}
			}
			writeDuration := time.Since(startWrite)
			if err = tx.Commit(); err != nil {
				if log.V(1) {
					log.Warning(err)
				}
				continue
			}
			if fromBalance >= amount {
				readings <- measurement{read: readDuration, write: writeDuration, total: time.Since(start)}
			}
		}
	}
}

func verifyBank(db *sql.DB) {
	var sum int
	if err := db.QueryRow("SELECT SUM(balance) FROM accounts").Scan(&sum); err != nil {
		log.Fatal(err)
	}
	if sum == *numAccounts*1000 {
		log.Info("The bank is in good order.")
	} else {
		log.Errorf("The bank is not in good order. Total value: %d", sum)
		os.Exit(1)
	}
}

func main() {
	flag.Parse()
	var db *sql.DB
	var err error
	var url string
	if *usePostgres {
		if db, err = sql.Open("postgres", "dbname=postgres sslmode=disable"); err != nil {
			log.Fatal(err)
		}
	} else {
		security.SetReadFileFn(securitytest.Asset)
		serv := server.StartTestServer(nil)
		defer serv.Stop()
		url = "https://root@" + serv.ServingAddr() + "?certs=test_certs"
		if db, err = sql.Open("cockroach", url); err != nil {
			log.Fatal(err)
		}
	}
	if _, err := db.Exec("CREATE DATABASE bank"); err != nil {
		if pqErr, ok := err.(*pq.Error); *usePostgres && (!ok || pqErr.Code.Name() != "duplicate_database") {
			log.Fatal(err)
		}
	}
	db.Close()

	// Open db client with database settings.
	if *usePostgres {
		if db, err = sql.Open("postgres", "dbname=bank sslmode=disable"); err != nil {
			log.Fatal(err)
		}
	} else {
		if db, err = sql.Open("cockroach", url+"&database=bank"); err != nil {
			log.Fatal(err)
		}
	}

	// concurrency + 1, for this thread and the "concurrency" number of
	// goroutines that move money
	db.SetMaxOpenConns(*concurrency + 1)

	if _, err = db.Exec("CREATE TABLE IF NOT EXISTS accounts (id BIGINT PRIMARY KEY, balance BIGINT NOT NULL)"); err != nil {
		log.Fatal(err)
	}

	if _, err = db.Exec("TRUNCATE TABLE accounts"); err != nil {
		log.Fatal(err)
	}

	for i := 0; i < *numAccounts; i++ {
		if _, err = db.Exec("INSERT INTO accounts (id, balance) VALUES ($1, $2)", i, 1000); err != nil {
			log.Fatal(err)
		}
	}

	verifyBank(db)

	lastNow := time.Now()
	readings := make(chan measurement, 10000)

	for i := 0; i < *concurrency; i++ {
		go moveMoney(db, readings)
	}

	for range time.NewTicker(*balanceCheckInterval).C {
		now := time.Now()
		elapsed := time.Since(lastNow)
		lastNow = now
		transfers := len(readings)
		log.Infof("%d transfers were executed at %.1f/second.", transfers, float64(transfers)/elapsed.Seconds())
		if transfers > 0 {
			var aggr measurement
			for i := 0; i < transfers; i++ {
				reading := <-readings
				aggr.read += reading.read
				aggr.write += reading.write
				aggr.total += reading.total
			}
			d := time.Duration(transfers)
			log.Infof("read time: %v, write time: %v, txn time: %v", aggr.read/d, aggr.write/d, aggr.total/d)
		}
		verifyBank(db)
	}
}

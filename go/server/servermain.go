// Copyright (C) 2018 Cranky Kernel
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package server

import (
	"encoding/json"
	"fmt"
	"github.com/crankykernel/binanceapi-go"
	"github.com/gorilla/mux"
	_ "github.com/mattn/go-sqlite3"
	"gitlab.com/crankykernel/maker/go/binanceex"
	"gitlab.com/crankykernel/maker/go/context"
	"gitlab.com/crankykernel/maker/go/db"
	"gitlab.com/crankykernel/maker/go/gencert"
	"gitlab.com/crankykernel/maker/go/log"
	"gitlab.com/crankykernel/maker/go/tradeservice"
	"gitlab.com/crankykernel/maker/go/version"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

var ServerFlags struct {
	Host           string
	Port           int16
	ConfigFilename string
	NoLog          bool
	OpenBrowser    bool
	DataDirectory  string
	TLS            bool
	ItsAllMyFault  bool
	EnableAuth     bool
}

func initBinanceExchangeInfoService() *binanceex.ExchangeInfoService {
	exchangeInfoService := binanceex.NewExchangeInfoService()
	if err := exchangeInfoService.Update(); err != nil {
		log.WithError(err).Errorf("Binance exchange info server failed to update")
	}
	go func() {
		for {
			time.Sleep(1 * time.Minute)
			if err := exchangeInfoService.Update(); err != nil {
				log.WithError(err).Errorf("Binance exchange info server failed to update")
			}
		}
	}()
	return exchangeInfoService
}

func ServerMain() {

	log.Infof("This is Maker version %s (git revision %s)",
		version.Version, version.GitRevision)

	log.SetLevel(log.LogLevelDebug)

	if _, err := os.Stat(ServerFlags.DataDirectory); err != nil {
		if err := os.Mkdir(ServerFlags.DataDirectory, 0700); err != nil {
			log.Fatalf("Failed to create data directory %s: %v", ServerFlags.DataDirectory, err)
		}
	}

	if !ServerFlags.NoLog {
		log.AddHook(log.NewFileOutputHook(path.Join(ServerFlags.DataDirectory, "maker.log")))
	}
	ServerFlags.ConfigFilename = path.Join(ServerFlags.DataDirectory, "maker.yaml")

	if ServerFlags.Host != "127.0.0.1" {
		if !ServerFlags.EnableAuth {
			log.Fatalf("Authentication must be enabled to listen on anything other than 127.0.0.1")
		}
		if !ServerFlags.TLS {
			log.Fatalf("TLS must be enabled to list on anything other than 127.0.0.1")
		}
		if !ServerFlags.ItsAllMyFault {
			log.Fatalf("Secret command line argument for non 127.0.0.1 listen not set. See documentation.")
		}
	}

	if ServerFlags.TLS {
		pemFilename := fmt.Sprintf("%s/maker.pem", ServerFlags.DataDirectory)
		if _, err := os.Stat(pemFilename); err != nil {
			gencert.GenCertMain(gencert.Flags{
				Host:     &gencert.DEFAULT_HOST,
				Org:      &gencert.DEFAULT_ORG,
				Filename: &pemFilename,
			}, []string{})
		}
	}

	applicationContext := &context.ApplicationContext{}
	applicationContext.BinanceTradeStreamManager = binanceex.NewXTradeStreamManager()

	db.DbOpen(ServerFlags.DataDirectory)

	tradeService := tradeservice.NewTradeService(applicationContext.BinanceTradeStreamManager)
	applicationContext.TradeService = tradeService

	restoreTrades(tradeService)

	binanceExchangeInfoService := initBinanceExchangeInfoService()
	binancePriceService := binanceex.NewBinancePriceService(binanceExchangeInfoService)

	applicationContext.BinanceUserDataStream = binanceex.NewBinanceUserDataStream()
	userStreamChannel := applicationContext.BinanceUserDataStream.Subscribe()
	go applicationContext.BinanceUserDataStream.Run()

	clientNoticeService := NewClientNoticeService()

	go func() {
		for {
			client := binanceapi.NewRestClient()
			requestStart := time.Now()
			response, err := client.GetTime()
			if err != nil {
				log.WithError(err).Errorf("Failed to get from Binance API")
				time.Sleep(1 * time.Minute)
				continue
			}

			roundTripTime := time.Now().Sub(requestStart)
			now := time.Now().UnixNano() / int64(time.Millisecond)
			diff := math.Abs(float64(now - response.ServerTime))
			if diff > 999 {
				log.WithFields(log.Fields{
					"roundTripTime":          roundTripTime,
					"binanceTimeDifferentMs": diff,
				}).Warnf("Time difference from Binance servers may be too large; order may fail")
				clientNoticeService.Broadcast(NewClientNotice(ClientNoticeLevelWarning,
					"Time difference between Binance and Maker server too large, orders may fail."))
			} else {
				log.WithFields(log.Fields{
					"roundTripTime":           roundTripTime,
					"binanceTimeDifferenceMs": diff,
				}).Infof("Binance time check")
			}
			time.Sleep(1 * time.Minute)
		}
	}()

	go func() {
		for {
			select {
			case event := <-userStreamChannel:
				switch event.EventType {
				case binanceex.EventTypeExecutionReport:
					if err := db.DbSaveBinanceRawExecutionReport(event.EventTime, event.Raw); err != nil {
						log.Println(err)
					}
					tradeService.OnExecutionReport(event)
				}
			}
		}
	}()

	router := mux.NewRouter()

	var authenticator *Authenticator = nil
	if ServerFlags.EnableAuth {
		authenticator = NewAuthenticator(ServerFlags.ConfigFilename)
		router.Use(authenticator.Middleware)
	}

	router.HandleFunc("/api/config", configHandler).Methods("GET")
	router.HandleFunc("/api/version", VersionHandler).Methods("GET")
	router.HandleFunc("/api/time", TimeHandler).Methods("GET")
	router.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		type LoginForm struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		var loginForm LoginForm
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&loginForm); err != nil {
			log.WithError(err).Errorf("Failed to decode login form")
			WriteJsonError(w, http.StatusInternalServerError, "error decoding login form")
			return
		}
		if authenticator == nil {
			WriteJsonResponse(w, http.StatusOK, map[string]interface{}{})
			return
		}
		sessionId, err := authenticator.Login(loginForm.Username, loginForm.Password)
		if err != nil {
			log.WithError(err).WithField("username", loginForm.Username).
				Errorf("Login failed")
			WriteJsonError(w, http.StatusUnauthorized, "authentication failed")
			return
		}
		log.Infof("SessionID: %s; Error: %v", sessionId, err)
		WriteJsonResponse(w, http.StatusOK, map[string]interface{}{
			"sessionId": sessionId,
		})
	})

	router.HandleFunc("/api/binance/buy", PostBuyHandler(tradeService, binancePriceService)).Methods("POST")
	router.HandleFunc("/api/binance/buy", deleteBuyHandler(tradeService)).Methods("DELETE")
	router.HandleFunc("/api/binance/sell", DeleteSellHandler(tradeService)).Methods("DELETE")

	// Set/change stop-loss on a trade.
	router.HandleFunc("/api/binance/trade/{tradeId}/stopLoss",
		updateTradeStopLossSettingsHandler(tradeService)).Methods("POST")

	router.HandleFunc("/api/binance/trade/{tradeId}/trailingProfit",
		updateTradeTrailingProfitSettingsHandler(tradeService)).Methods("POST")

	// Limit sell at percent.
	router.HandleFunc("/api/binance/trade/{tradeId}/limitSellByPercent",
		limitSellByPercentHandler(tradeService)).Methods("POST")

	// Limit sell at price.
	router.HandleFunc("/api/binance/trade/{tradeId}/limitSellByPrice",
		limitSellByPriceHandler(tradeService)).Methods("POST")

	router.HandleFunc("/api/binance/trade/{tradeId}/marketSell",
		marketSellHandler(tradeService)).Methods("POST")
	router.HandleFunc("/api/binance/trade/{tradeId}/archive",
		archiveTradeHandler(tradeService)).Methods("POST")
	router.HandleFunc("/api/binance/trade/{tradeId}/abandon",
		abandonTradeHandler(tradeService)).Methods("POST")

	router.HandleFunc("/api/trade/query", queryTradesHandler).
		Methods("GET")
	router.HandleFunc("/api/trade/{tradeId}",
		getTradeHandler).Methods("GET")

	router.HandleFunc("/api/binance/account/test",
		BinanceTestHandler).Methods("GET")
	router.HandleFunc("/api/binance/config",
		SaveBinanceConfigHandler).Methods("POST")
	router.HandleFunc("/api/config/preferences",
		SavePreferencesHandler).Methods("POST")

	binanceApiProxyHandler := http.StripPrefix("/proxy/binance",
		binanceapi.NewBinanceApiProxyHandler())
	router.PathPrefix("/proxy/binance").Handler(binanceApiProxyHandler)

	router.PathPrefix("/ws").Handler(NewUserWebSocketHandler(applicationContext, clientNoticeService))

	router.PathPrefix("/").HandlerFunc(staticAssetHandler())

	listenHostPort := fmt.Sprintf("%s:%d", ServerFlags.Host, ServerFlags.Port)
	log.Printf("Starting server on %s.", listenHostPort)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {

		var err error = nil

		if ServerFlags.TLS {
			pemFilename := fmt.Sprintf("%s/maker.pem", ServerFlags.DataDirectory)
			err = http.ListenAndServeTLS(listenHostPort, pemFilename, pemFilename, router)
		} else {
			err = http.ListenAndServe(listenHostPort, router)
		}

		if err != nil {
			log.Fatal("Failed to start server: ", err)
		}
	}()

	if ServerFlags.OpenBrowser {
		url := fmt.Sprintf("http://%s:%d", ServerFlags.Host, ServerFlags.Port)
		log.Info("Attempting to start browser.")
		go func() {
			if runtime.GOOS == "linux" {
				c := exec.Command("xdg-open", url)
				c.Run()
			} else if runtime.GOOS == "darwin" {
				c := exec.Command("open", url)
				c.Run()
			} else if runtime.GOOS == "windows" {
				cmd := "url.dll,FileProtocolHandler"
				runDll32 := filepath.Join(os.Getenv("SYSTEMROOT"), "System32",
					"rundll32.exe")
				c := exec.Command(runDll32, cmd, url)
				if err := c.Run(); err != nil {
					log.WithError(err).WithFields(log.Fields{
						"os": "windows",
					}).Errorf("Failed to start browser.")
				}
			}
		}()
	}

	wg.Wait()
}

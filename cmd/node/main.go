// Copyright (c) 2022 Blockwatch Data Inc.
// Author: alex@blockwatch.cc

package main

import (
    "encoding/base64"
    "encoding/json"
    "flag"
    "fmt"
    "io/ioutil"
    "math/big"
    "net/http"
    "os"
    "path/filepath"
    "time"

    "blockwatch.cc/near-api-go"
    "github.com/echa/log"
    cid "github.com/ipfs/go-cid"
)

var (
    contractAddress string
    networkId       string
    accountId       string
    databaseId      string
    rpcEndpoint     string
    port            string
    conn            *near.Connection
    flags           = flag.NewFlagSet("node", flag.ContinueOnError)
    home            string
    account         *near.Account
)

func init() {
    flags.Usage = func() {}
    flags.StringVar(&contractAddress, "contract", os.Getenv("DB3_CONTRACT_ID"), "DB3 contract")
    flags.StringVar(&accountId, "account", os.Getenv("DB3_NODE_ACCOUNT_ID"), "DB3 node account")
    flags.StringVar(&databaseId, "db", "0", "DB3 database id to host")
    flags.StringVar(&rpcEndpoint, "rpc", "https://rpc.testnet.near.org", "NEAR RPC endpoint")
    flags.StringVar(&networkId, "net", "testnet", "NEAR network id")
    flags.StringVar(&port, "port", "8000", "HTTP server port")

    var err error
    home, err = os.UserHomeDir()
    if err != nil {
        panic(err)
    }
}

func main() {
    if err := run(); err != nil {
        log.Fatalf("Error: %v\n", err)
    }
}

func run() error {
    err := flags.Parse(os.Args[1:])
    if err != nil {
        if err == flag.ErrHelp {
            fmt.Printf("Usage: %s [flags]\n", os.Args[0])
            fmt.Println("\nFlags")
            flags.PrintDefaults()
            return nil
        }
        return err
    }

    if accountId == "" {
        return fmt.Errorf("Empty account id")
    }
    if contractAddress == "" {
        return fmt.Errorf("Empty contract id")
    }

    conn = near.NewConnection(rpcEndpoint)

    // Use a key pair directly as a signer.
    log.Infof("Loading account %s", accountId)
    cfg := &near.Config{
        NetworkID: networkId,
        NodeURL:   rpcEndpoint,
        KeyPath:   filepath.Join(home, ".near-credentials", networkId, accountId+".json"),
    }
    account, err = near.LoadAccount(conn, cfg, accountId)
    if err != nil {
        return err
    }

    if err := initDatabase(); err != nil {
        return err
    }

    // use default http server
    log.Infof("Listening on :%s", port)
    http.HandleFunc("/", queryHandler)
    return http.ListenAndServe(":"+port, nil)
}

type Manifest struct {
    Author      string `json:"author_id"`
    Name        string `json:"name"`
    License     string `json:"license"`
    Cid         string `json:"code_cid"`
    RoyaltyBips int    `json:"royalty_bips,string"`
}

type SignedQuery struct {
    Db    string `json:"db"`
    Query string `json:"query"`
    Cid   string `json:"cid"`
    FeeTx []byte `json:"fee_tx"`
}

type SignedResult struct {
    QueryCID  string      `json:"query_cid"`
    ResultCID string      `json:"result_cid"`
    Result    interface{} `json:"result"`
    Sig       string      `json:"sig"`
}

func queryHandler(w http.ResponseWriter, r *http.Request) {
    // check method
    if r.Method != http.MethodPost {
        http.Error(w, "invalid method", http.StatusMethodNotAllowed)
        return
    }

    // parse query
    var query SignedQuery
    dec := json.NewDecoder(r.Body)
    err := dec.Decode(&query)
    if err != nil {
        log.Error(err)
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    r.Body.Close()

    c, err := cid.Decode(query.Cid)
    if err != nil {
        log.Error(err)
        http.Error(w, fmt.Sprintf("invalid cid: %v", err), http.StatusBadRequest)
        return
    }

    // TODO: check embedded transaction is valid and signed

    // execute DB query
    result, err := executeQuery(query)
    if err != nil {
        log.Error(err)
        http.Error(w, fmt.Sprintf("query failed: %v", err), http.StatusInternalServerError)
        return
    }

    // create result CID
    buf, err := json.Marshal(result)
    if err != nil {
        log.Error(err)
        http.Error(w, fmt.Sprintf("marshal result: %v", err), http.StatusInternalServerError)
        return
    }
    c, err = c.Prefix().Sum(buf)
    if err != nil {
        log.Error(err)
        http.Error(w, fmt.Sprintf("encode cid: %v", err), http.StatusInternalServerError)
        return
    }

    // create result CID and return it to the user
    response := SignedResult{
        QueryCID:  query.Cid,
        ResultCID: c.String(),
        Result:    result,
        Sig:       "TODO",
    }
    buf, err = json.Marshal(response)
    if err != nil {
        log.Error(err)
        http.Error(w, fmt.Sprintf("marshal response: %v", err), http.StatusInternalServerError)
        return
    }

    w.Header().Set("Content-Type", "application/json")
    w.Header().Set("Date", time.Now().Format(http.TimeFormat))
    w.WriteHeader(http.StatusOK)
    w.Write(buf)

    // sign settle call and broadcast embedded fee tx async
    go func() {
        // fee tx
        for retries := 3; retries > 0; retries-- {
            log.Infof("Broadcasting user tx")
            res, err := conn.SendTransactionAsync(query.FeeTx)
            if err == nil {
                log.Infof("Result: %s", res)
                break
            } else {
                log.Error(err)
                <-time.After(time.Second)
            }
        }

        // sign and broadcast settle
        args, _ := json.Marshal(map[string]string{
            "dbid": query.Db,
            "qid":  query.Cid,
            "rid":  c.String(),
        })
        for retries := 3; retries > 0; retries-- {
            log.Infof("Sending settle call with args %s", string(args))
            res, err := account.FunctionCall(
                contractAddress,
                "settle",
                args,
                100_000_000_000_000,
                *big.NewInt(0),
            )
            if err == nil {
                if r, err := handleResult(res); err != nil {
                    log.Errorf("%v", err)
                } else {
                    log.Infof("Result: %s", string(r))
                }
                break
            } else {
                log.Error(err)
                <-time.After(time.Second)
            }
        }
    }()
}

func handleResult(res map[string]interface{}) ([]byte, error) {
    // buf, _ := json.MarshalIndent(res, "", "  ")
    // log.Infof("Res %s", string(buf))
    success := res["status"].(map[string]interface{})["SuccessValue"]
    failed := res["status"].(map[string]interface{})["Failure"]
    if success == nil {
        buf, _ := json.Marshal(failed.(map[string]interface{}))
        return nil, fmt.Errorf("%s", string(buf))
    }
    return base64.StdEncoding.DecodeString(success.(string))
}

func executeQuery(query SignedQuery) (interface{}, error) {
    log.Infof("Processing query db=%s cid=%s q=%q", query.Db, query.Cid, query.Query)

    // TODO: execute query against a real database

    return "Wsup?", nil
}

func initDatabase() error {
    // load the database manifest
    log.Infof("Loading database %s id %s", contractAddress, databaseId)
    res, err := account.FunctionCall(
        contractAddress,
        "manifest",
        []byte(`{"dbid":"`+databaseId+`"}`),
        100_000_000_000_000,
        *big.NewInt(0),
    )
    if err != nil {
        return err
    }
    buf, err := base64.StdEncoding.DecodeString(res["status"].(map[string]interface{})["SuccessValue"].(string))
    if err != nil {
        return err
    }
    var m Manifest
    if err := json.Unmarshal(buf, &m); err != nil {
        return err
    }
    log.Infof("> %#v", m)

    log.Infof("Initializing database from cid=%s", m.Cid)
    resp, err := http.Get("https://ipfs.io/ipfs/" + m.Cid)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    sql, err := ioutil.ReadAll(resp.Body)
    if err != nil {
        return err
    }
    log.Infof("SQL\n%s", string(sql))

    // TODO: connect and init a real database here

    return nil
}

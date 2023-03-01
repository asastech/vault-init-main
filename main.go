package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/cloudkms/v1"
	"google.golang.org/api/option"
)

var (
	vaultAddr     string
	checkInterval string
	gcsBucketName string
	httpClient    http.Client

	kmsService *cloudkms.Service
	kmsKeyId   string

	storageClient *storage.Client

	userAgent = fmt.Sprintf("vault-init/1.0.0 (%s)", runtime.Version())
)

// InitRequest holds a Vault init request.
type InitRequest struct {
	SecretShares    int `json:"secret_shares"`
	SecretThreshold int `json:"secret_threshold"`
}

// InitResponse holds a Vault init response.
type InitResponse struct {
	Keys       []string `json:"keys"`
	KeysBase64 []string `json:"keys_base64"`
	RootToken  string   `json:"root_token"`
}

// UnsealRequest holds a Vault unseal request.
type UnsealRequest struct {
	Key   string `json:"key"`
	Reset bool   `json:"reset"`
}

// UnsealResponse holds a Vault unseal response.
type UnsealResponse struct {
	Sealed   bool `json:"sealed"`
	T        int  `json:"t"`
	N        int  `json:"n"`
	Progress int  `json:"progress"`
}

func main() {
	log.Println("Starting the vault-init service...")

	vaultAddr = os.Getenv("VAULT_ADDR")
	if vaultAddr == "" {
		vaultAddr = "https://127.0.0.1:8200"
	}

	checkInterval = os.Getenv("CHECK_INTERVAL")
	if checkInterval == "" {
		checkInterval = "10"
	}

	i, err := strconv.Atoi(checkInterval)
	if err != nil {
		log.Fatalf("CHECK_INTERVAL is invalid: %s", err)
	}

	checkIntervalDuration := time.Duration(i) * time.Second

	gcsBucketName = os.Getenv("GCS_BUCKET_NAME")
	if gcsBucketName == "" {
		log.Fatal("GCS_BUCKET_NAME must be set and not empty")
	}

	kmsKeyId = os.Getenv("KMS_KEY_ID")
	if kmsKeyId == "" {
		log.Fatal("KMS_KEY_ID must be set and not empty")
	}

	kmsCtx, kmsCtxCancel := context.WithCancel(context.Background())
	defer kmsCtxCancel()
	kmsClient, err := google.DefaultClient(kmsCtx, "https://www.googleapis.com/auth/cloudkms")
	if err != nil {
		log.Println(err)
		return
	}

	kmsService, err = cloudkms.New(kmsClient)
	if err != nil {
		log.Println(err)
		return
	}
	kmsService.UserAgent = userAgent

	storageCtx, storageCtxCancel := context.WithCancel(context.Background())
	defer storageCtxCancel()
	storageClient, err = storage.NewClient(storageCtx,
		option.WithUserAgent(userAgent),
		option.WithScopes(storage.ScopeReadWrite),
	)
	if err != nil {
		log.Fatal(err)
	}

	httpClient = http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	signalCh := make(chan os.Signal)
	signal.Notify(signalCh,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGKILL,
	)

	stop := func() {
		log.Printf("Shutting down")
		kmsCtxCancel()
		storageCtxCancel()
		os.Exit(0)
	}

	for {
		select {
		case <-signalCh:
			stop()
		default:
		}
		response, err := httpClient.Head(vaultAddr + "/v1/sys/health")

		if response != nil && response.Body != nil {
			response.Body.Close()
		}

		if err != nil {
			log.Println(err)
			time.Sleep(checkIntervalDuration)
			continue
		}

		switch response.StatusCode {
		case 200:
			log.Println("Vault is initialized and unsealed.")
		case 429:
			log.Println("Vault is unsealed and in standby mode.")
		case 501:
			log.Println("Vault is not initialized. Initializing and unsealing...")
			initialize()
			unseal()
		case 503:
			log.Println("Vault is sealed. Unsealing...")
			unseal()
		default:
			log.Printf("Vault is in an unknown state. Status code: %d", response.StatusCode)
		}

		log.Printf("Next check in %s", checkIntervalDuration)

		select {
		case <-signalCh:
			stop()
		case <-time.After(checkIntervalDuration):
		}
	}
}

func initialize() {
	initRequest := InitRequest{
		SecretShares:    5,
		SecretThreshold: 3,
	}

	initRequestData, err := json.Marshal(&initRequest)
	if err != nil {
		log.Println(err)
		return
	}

	r := bytes.NewReader(initRequestData)
	request, err := http.NewRequest("PUT", vaultAddr+"/v1/sys/init", r)
	if err != nil {
		log.Println(err)
		return
	}

	response, err := httpClient.Do(request)
	if err != nil {
		log.Println(err)
		return
	}
	defer response.Body.Close()

	initRequestResponseBody, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Println(err)
		return
	}

	if response.StatusCode != 200 {
		log.Printf("init: non 200 status code: %d", response.StatusCode)
		return
	}

	var initResponse InitResponse

	if err := json.Unmarshal(initRequestResponseBody, &initResponse); err != nil {
		log.Println(err)
		return
	}

	log.Println("Encrypting unseal keys and the root token...")

	rootTokenEncryptRequest := &cloudkms.EncryptRequest{
		Plaintext: base64.StdEncoding.EncodeToString([]byte(initResponse.RootToken)),
	}

	rootTokenEncryptResponse, err := kmsService.Projects.Locations.KeyRings.CryptoKeys.Encrypt(kmsKeyId, rootTokenEncryptRequest).Do()
	if err != nil {
		log.Println(err)
		return
	}

	unsealKeysEncryptRequest := &cloudkms.EncryptRequest{
		Plaintext: base64.StdEncoding.EncodeToString(initRequestResponseBody),
	}

	unsealKeysEncryptResponse, err := kmsService.Projects.Locations.KeyRings.CryptoKeys.Encrypt(kmsKeyId, unsealKeysEncryptRequest).Do()
	if err != nil {
		log.Println(err)
		return
	}

	bucket := storageClient.Bucket(gcsBucketName)

	// Save the encrypted unseal keys.
	ctx := context.Background()
	unsealKeysObject := bucket.Object("unseal-keys.json.enc").NewWriter(ctx)
	defer unsealKeysObject.Close()

	_, err = unsealKeysObject.Write([]byte(unsealKeysEncryptResponse.Ciphertext))
	if err != nil {
		log.Println(err)
	}

	log.Printf("Unseal keys written to gs://%s/%s", gcsBucketName, "unseal-keys.json.enc")

	// Save the encrypted root token.
	rootTokenObject := bucket.Object("root-token.enc").NewWriter(ctx)
	defer rootTokenObject.Close()

	_, err = rootTokenObject.Write([]byte(rootTokenEncryptResponse.Ciphertext))
	if err != nil {
		log.Println(err)
	}

	log.Printf("Root token written to gs://%s/%s", gcsBucketName, "root-token.enc")

	log.Println("Initialization complete.")
}

func unseal() {
	bucket := storageClient.Bucket(gcsBucketName)

	ctx := context.Background()
	unsealKeysObject, err := bucket.Object("unseal-keys.json.enc").NewReader(ctx)
	if err != nil {
		log.Println(err)
		return
	}

	defer unsealKeysObject.Close()

	unsealKeysData, err := ioutil.ReadAll(unsealKeysObject)
	if err != nil {
		log.Println(err)
		return
	}

	unsealKeysDecryptRequest := &cloudkms.DecryptRequest{
		Ciphertext: string(unsealKeysData),
	}

	unsealKeysDecryptResponse, err := kmsService.Projects.Locations.KeyRings.CryptoKeys.Decrypt(kmsKeyId, unsealKeysDecryptRequest).Do()
	if err != nil {
		log.Println(err)
		return
	}

	var initResponse InitResponse

	unsealKeysPlaintext, err := base64.StdEncoding.DecodeString(unsealKeysDecryptResponse.Plaintext)
	if err != nil {
		log.Println(err)
		return
	}

	if err := json.Unmarshal(unsealKeysPlaintext, &initResponse); err != nil {
		log.Println(err)
		return
	}

	for _, key := range initResponse.KeysBase64 {
		done, err := unsealOne(key)
		if done {
			return
		}

		if err != nil {
			log.Println(err)
			return
		}
	}
}

func unsealOne(key string) (bool, error) {
	unsealRequest := UnsealRequest{
		Key: key,
	}

	unsealRequestData, err := json.Marshal(&unsealRequest)
	if err != nil {
		return false, err
	}

	r := bytes.NewReader(unsealRequestData)
	request, err := http.NewRequest(http.MethodPut, vaultAddr+"/v1/sys/unseal", r)
	if err != nil {
		return false, err
	}

	response, err := httpClient.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()

	if response.StatusCode != 200 {
		return false, fmt.Errorf("unseal: non-200 status code: %d", response.StatusCode)
	}

	unsealRequestResponseBody, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return false, err
	}

	var unsealResponse UnsealResponse
	if err := json.Unmarshal(unsealRequestResponseBody, &unsealResponse); err != nil {
		return false, err
	}

	if !unsealResponse.Sealed {
		return true, nil
	}

	return false, nil
}

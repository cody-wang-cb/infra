package proxyd

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"
)

func TestStripXFF(t *testing.T) {
	tests := []struct {
		in, out string
	}{
		{"1.2.3, 4.5.6, 7.8.9", "1.2.3"},
		{"1.2.3,4.5.6", "1.2.3"},
		{" 1.2.3 , 4.5.6 ", "1.2.3"},
	}

	for _, test := range tests {
		actual := stripXFF(test.in)
		assert.Equal(t, test.out, actual)
	}
}

func TestExecuteMulticallTransactionLogging(t *testing.T) {
	// Create a mock backend server that returns a successful response
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Return a successful transaction hash response
		response := `{"jsonrpc":"2.0","result":"0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef","id":1}`
		w.Write([]byte(response))
	}))
	defer server.Close()

	// Create backends
	backend1 := NewBackend("backend1", server.URL, "", semaphore.NewWeighted(1))
	backend2 := NewBackend("backend2", server.URL, "", semaphore.NewWeighted(1))

	// Create backend group with multicall routing
	backendGroup := &BackendGroup{
		Name:                   "test-group",
		Backends:               []*Backend{backend1, backend2},
		routingStrategy:        MulticallRoutingStrategy,
		multicallRPCErrorCheck: false,
	}

	// Create a proper transaction to the dev wallet address
	devWalletAddr := common.HexToAddress("0x889766967Dd3FF6A0C91b799322D45628e68F8b1")

	// Create a legacy transaction (simple and works reliably)
	tx := types.NewTransaction(
		0,                               // nonce
		devWalletAddr,                   // to
		big.NewInt(1000000000000000000), // value (1 ETH)
		21000,                           // gas limit
		big.NewInt(20000000000),         // gas price (20 gwei)
		nil,                             // data
	)

	// Sign the transaction with a dummy private key for testing
	privateKey, err := crypto.GenerateKey()
	require.NoError(t, err)

	signer := types.HomesteadSigner{}
	signedTx, err := types.SignTx(tx, signer, privateKey)
	require.NoError(t, err)

	// Encode the transaction
	txBytes, err := signedTx.MarshalBinary()
	require.NoError(t, err)

	devWalletTxHex := hexutil.Encode(txBytes)

	// Create sendRawTransaction request
	rpcReq := &RPCReq{
		JSONRPC: "2.0",
		Method:  "eth_sendRawTransaction",
		Params:  json.RawMessage(fmt.Sprintf(`["%s"]`, devWalletTxHex)),
		ID:      json.RawMessage(`1`),
	}

	// First, let's verify this transaction actually parses to the dev wallet address
	t.Run("verify transaction parsing", func(t *testing.T) {
		var params []any
		err := json.Unmarshal(rpcReq.Params, &params)
		require.NoError(t, err)
		require.Len(t, params, 1)

		txDataHex, ok := params[0].(string)
		require.True(t, ok)

		data, err := hexutil.Decode(txDataHex)
		require.NoError(t, err)

		tx := new(types.Transaction)
		err = tx.UnmarshalBinary(data)
		require.NoError(t, err)

		// Verify this transaction goes to the dev wallet
		require.NotNil(t, tx.To())
		devWalletAddr := "0x889766967dd3ff6a0c91b799322d45628e68f8b1"
		require.Equal(t, strings.ToLower(devWalletAddr), strings.ToLower(tx.To().Hex()))

		t.Logf("Transaction hash: %s", tx.Hash().Hex())
		t.Logf("Transaction to address: %s", tx.To().Hex())
	})

	ctx := context.Background()

	// Execute multicall - this should hit line 1051 in backend.go
	result := backendGroup.ExecuteMulticall(ctx, []*RPCReq{rpcReq})

	// Verify the response structure is correct
	assert.NotNil(t, result)
	assert.NoError(t, result.error)
	assert.NotNil(t, result.RPCRes)
	assert.Len(t, result.RPCRes, 1)

	// Verify the result contains a transaction hash
	assert.NotNil(t, result.RPCRes[0].Result)
}

func TestExecuteMulticallNonDevWalletTransaction(t *testing.T) {
	// Create a mock backend server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		response := `{"jsonrpc":"2.0","result":"0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef","id":1}`
		w.Write([]byte(response))
	}))
	defer server.Close()

	backend := NewBackend("backend1", server.URL, "", semaphore.NewWeighted(1))
	backendGroup := &BackendGroup{
		Name:                   "test-group",
		Backends:               []*Backend{backend},
		routingStrategy:        MulticallRoutingStrategy,
		multicallRPCErrorCheck: false,
	}

	// Test transaction to a different address (not dev wallet)
	// This transaction goes to 0xf80267194936da1e98db10bce06f3147d580a62e (different from dev wallet)
	nonDevWalletTxHex := "0x02f8748201a415843b9aca31843b9aca3182520894f80267194936da1e98db10bce06f3147d580a62e880de0b6b3a764000080c001a0b50ee053102360ff5fedf0933b912b7e140c90fe57fa07a0cebe70dbd72339dda072974cb7bfe5c3dc54dde110e2b049408ccab8a879949c3b4d42a3a7555a618b"

	rpcReq := &RPCReq{
		JSONRPC: "2.0",
		Method:  "eth_sendRawTransaction",
		Params:  json.RawMessage(fmt.Sprintf(`["%s"]`, nonDevWalletTxHex)),
		ID:      json.RawMessage(`1`),
	}

	ctx := context.Background()
	result := backendGroup.ExecuteMulticall(ctx, []*RPCReq{rpcReq})

	// Verify basic functionality works for non-dev wallet transactions
	assert.NotNil(t, result)
	assert.NoError(t, result.error)
	assert.NotNil(t, result.RPCRes)
	assert.Len(t, result.RPCRes, 1)
}

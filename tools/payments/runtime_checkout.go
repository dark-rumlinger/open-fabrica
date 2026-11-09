package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"openfabrica/internal/funcexec"
	"openfabrica/tools"
)

// The runtime half of hosted checkout. A buyer's action reaches the app's own API (a
// manage_function endpoint); its handler calls ctx.checkout(...), which runs the same
// create_checkout code the build-time tool runs. That matters for fulfilment: the
// payments row written here is what POST /payments/webhook reconciles into
// payment.completed, so a handler that opened a session through ctx.fetch instead
// produced a payment the platform never recognises.

func init() { funcexec.RegisterCheckout(runtimeCheckout) }

func runtimeCheckout(rt funcexec.Runtime, args map[string]any) (map[string]any, error) {
	// Round-trip through JSON so the JS values arrive in the shapes a tool call's args
	// have (numbers as float64, objects as maps): the sandbox hands over int64s.
	var norm map[string]any
	if b, err := json.Marshal(args); err != nil || json.Unmarshal(b, &norm) != nil {
		return nil, errors.New("ctx.checkout: the argument must be a plain object")
	}
	tc := &tools.ToolContext{
		Ctx:       context.Background(),
		DB:        rt.Writer,
		GlobalDB:  rt.GlobalDB,
		ProjectID: rt.ProjectID,
		Encryptor: rt.Enc,
	}
	// A plan_id makes it a recurring subscription checkout (manage_subscriptions
	// create); otherwise a one-off payment (manage_payments create_checkout).
	var action tools.Action
	if _, recurring := norm["plan_id"]; recurring {
		var sub SubscriptionCreate
		if err := tools.PopulateAction(&sub, norm); err != nil {
			return nil, fmt.Errorf("ctx.checkout: %w", err)
		}
		action = &sub
	} else {
		var one PaymentCreateCheckout
		if err := tools.PopulateAction(&one, norm); err != nil {
			return nil, fmt.Errorf("ctx.checkout: %w", err)
		}
		action = &one
	}
	if s, _ := norm["success_url"].(string); s == "" {
		return nil, errors.New("ctx.checkout needs success_url and cancel_url, plus amount (cents) or plan_id")
	}
	res, err := action.Run(tc)
	if err != nil {
		return nil, fmt.Errorf("ctx.checkout: %w", err)
	}
	if !res.Success {
		return nil, fmt.Errorf("ctx.checkout: %s", res.Error)
	}
	data, _ := res.Data.(map[string]any)
	return data, nil
}

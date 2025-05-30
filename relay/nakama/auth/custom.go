package auth

import (
	"context"
	"database/sql"
	"errors"
	"os"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/rotisserie/eris"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcode "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	signInWithEthereumType = "siwe"
	signInWithArgusIDType  = "argus"
	envJWTSecret           = "JWT_SECRET"
)

var (
	ErrBadCustomAuthType = errors.New("bad custom auth type")
	GlobalJWTSecret      string
)

func checkJWTSecret(logger runtime.Logger) {
	if GlobalJWTSecret == "" {
		GlobalJWTSecret = os.Getenv(envJWTSecret)
	}
	if GlobalJWTSecret == "" {
		logger.Warn("JWT secret isn't set. You won't be able to use Argus ID custom link")
	}
}

// Now we can only use symmetric JWTs, which means that the custom Argus ID authentication can only
// be used for our (Argus) projects because we can't share the JWT secret. Instead of failing
// during initialization if the JWT secret isn't set, we'll log a warning message and return a
// ErrBadCustomAuthType if a client tries to use Argus ID custom authentication.
//
// When Supabase rolls out asymmetric JWTs, the JWT secret can be shared because it is essentially a
// public key. If the JWT_SECRET variable isn't set, we can consider fetching a valid public key
// and set it as the value of JWT_SECRET. This way, Argus ID authentication will always be enabled
// (assuming the fetch didn't fail), and we don't have to return an error.
func InitCustomAuthentication(logger runtime.Logger, initializer runtime.Initializer) error {
	if err := initializer.RegisterBeforeAuthenticateCustom(handleCustomAuthentication); err != nil {
		return eris.Wrap(err, "failed to init custom authentication")
	}
	checkJWTSecret(logger)
	return nil
}

func handleCustomAuthentication(
	ctx context.Context,
	logger runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	in *api.AuthenticateCustomRequest) (*api.AuthenticateCustomRequest, error) {
	authType := in.GetAccount().GetVars()["type"]
	ctx, span := otel.Tracer("nakama.auth").Start(ctx, "AuthenticateCustom",
		trace.WithAttributes(
			attribute.String("type", authType),
		))
	defer span.End()
	// In the future, other authentication methods can be added here (e.g. Twitter)
	if authType == signInWithEthereumType {
		ctxSiwe, spanSiwe := otel.Tracer("nakama.auth").Start(ctx, "SIWE Custom Auth")
		defer spanSiwe.End()
		inResult, err := authWithSIWE(ctxSiwe, logger, nk, in)
		if err != nil {
			spanSiwe.RecordError(err)
			spanSiwe.SetStatus(otelcode.Error, "Failed to authenticate with SIWE")
			return nil, err
		}
		spanSiwe.SetStatus(otelcode.Ok, "Successfully authenticated with SIWE")
		return inResult, nil
	}
	if authType == signInWithArgusIDType && GlobalJWTSecret != "" {
		ctxArgus, spanArgus := otel.Tracer("nakama.auth").Start(ctx, "Argus ID Custom Auth")
		defer spanArgus.End()
		inResult, err := authWithArgusID(ctxArgus, logger, nk, in)
		if err != nil {
			spanArgus.RecordError(err)
			spanArgus.SetStatus(otelcode.Error, "Failed to authenticate with Argus ID")
			return nil, err
		}
		spanArgus.SetStatus(otelcode.Ok, "Successfully authenticated with Argus ID")
		return inResult, nil
	}
	span.RecordError(ErrBadCustomAuthType)
	span.SetStatus(otelcode.Error, "Bad custom auth type")
	return nil, ErrBadCustomAuthType
}

func InitCustomLink(logger runtime.Logger, initializer runtime.Initializer) error {
	if err := initializer.RegisterBeforeLinkCustom(handleCustomLink); err != nil {
		return eris.Wrap(err, "failed to init custom link")
	}
	checkJWTSecret(logger)
	return nil
}

func handleCustomLink(
	ctx context.Context,
	logger runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	in *api.AccountCustom) (*api.AccountCustom, error) {
	authType := in.GetVars()["type"]
	ctx, span := otel.Tracer("nakama.auth").Start(ctx, "LinkCustom",
		trace.WithAttributes(
			attribute.String("type", authType),
		))
	defer span.End()
	// In the future, other authentication methods can be added here (e.g. Twitter)
	if authType == signInWithEthereumType {
		ctxSiwe, spanSiwe := otel.Tracer("nakama.auth").Start(ctx, "SIWE Custom Link")
		defer spanSiwe.End()
		inResult, err := linkWithSIWE(ctxSiwe, logger, nk, in)
		if err != nil {
			spanSiwe.RecordError(err)
			spanSiwe.SetStatus(otelcode.Error, "Failed to link with SIWE")
			return nil, err
		}
		spanSiwe.SetStatus(otelcode.Ok, "Successfully linked with SIWE")
		return inResult, nil
	}
	if authType == signInWithArgusIDType && GlobalJWTSecret != "" {
		ctxArgus, spanArgus := otel.Tracer("nakama.auth").Start(ctx, "Argus ID Custom Link")
		defer spanArgus.End()
		inResult, err := linkWithArgusID(ctxArgus, logger, nk, in)
		if err != nil {
			spanArgus.RecordError(err)
			spanArgus.SetStatus(otelcode.Error, "Failed to link with Argus ID")
			return nil, err
		}
		spanArgus.SetStatus(otelcode.Ok, "Successfully linked with Argus ID")
		return inResult, nil
	}
	span.RecordError(ErrBadCustomAuthType)
	span.SetStatus(otelcode.Error, "Bad custom auth type")
	return nil, ErrBadCustomAuthType
}

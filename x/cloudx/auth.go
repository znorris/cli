package cloudx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gofrs/uuid/v3"
	kratos "github.com/ory/kratos-client-go"
	"github.com/ory/x/cmdx"
	"github.com/ory/x/flagx"
	"github.com/ory/x/stringsx"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/tidwall/gjson"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
)

const (
	fileName   = ".ory-cloud.json"
	configFlag = "cloud-config"
	quietFlag  = "quiet"
	yesFlag    = "yes"
	osEnvVar   = "ORY_CLOUD_CONFIG_PATH"
	cloudUrl   = "ORY_CLOUD_URL"
	version    = "v0alpha0"
)

func RegisterFlags(f *pflag.FlagSet) {
	f.String(configFlag, "", "Path to the Ory Cloud configuration file.")
	f.Bool(quietFlag, false, "Do not print any output.")
	f.Bool(yesFlag, false, "Do not ask for confirmation.")
}

type AuthContext struct {
	Version         string       `json:"version"`
	SessionToken    string       `json:"session_token"`
	SelectedProject uuid.UUID    `json:"selected_project"`
	IdentityTraits  AuthIdentity `json:"session_identity_traits"`
}

type AuthIdentity struct {
	ID    uuid.UUID
	Email string `json:"email"`
}

type AuthProject struct {
	ID   uuid.UUID `json:"id"`
	Slug string    `json:"slug"`
}

var ErrNoConfig = errors.New("no ory configuration file present")

func getConfigPath(cmd *cobra.Command) (string, error) {
	path, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrapf(err, "unable to guess your home directory")
	}

	return stringsx.Coalesce(
		os.Getenv(osEnvVar),
		flagx.MustGetString(cmd, configFlag),
		filepath.Join(path, fileName),
	), nil
}

type Auth struct {
	ctx            context.Context
	verboseWriter  io.Writer
	configLocation string
	noConfirm      bool
	apiDomain      *url.URL
}

func NewHandler(cmd *cobra.Command) (*Auth, error) {
	location, err := getConfigPath(cmd)
	if err != nil {
		return nil, err
	}

	var out io.Writer = os.Stdout
	if flagx.MustGetBool(cmd, quietFlag) {
		out = io.Discard
	}

	toParse := stringsx.Coalesce(
		os.Getenv(cloudUrl),
		"https://project.console.ory.sh",
	)
	apiDomain, err := url.Parse(toParse)
	if err != nil {
		return nil, errors.Wrapf(err, "invalid API endpoint provided: %s", toParse)
	}

	return &Auth{
		configLocation: location,
		noConfirm:      flagx.MustGetBool(cmd, yesFlag),
		verboseWriter:  out,
		apiDomain:      apiDomain,
		ctx:            cmd.Context(),
	}, nil
}

func (h *Auth) WriteConfig(c *AuthContext) error {
	c.Version = version
	file, err := os.OpenFile(h.configLocation, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return errors.Wrapf(err, "unable to open file for writing at location: %s", file)
	}
	defer file.Close()

	if err := json.NewEncoder(file).Encode(c); err != nil {
		return errors.Wrapf(err, "unable to write configuration to file: %s", h.configLocation)
	}

	return nil
}

func (h *Auth) readConfig() (*AuthContext, error) {
	file, err := os.Open(h.configLocation)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return new(AuthContext), ErrNoConfig
		}
		return nil, errors.Wrapf(err, "unable to open ory config file location: %s", h.configLocation)
	}
	defer file.Close()

	var c AuthContext
	if err := json.NewDecoder(file).Decode(&c); err != nil {
		return nil, errors.Wrapf(err, "unable to JSON decode the ory config file: %s", h.configLocation)
	}

	return &c, nil
}

func (h *Auth) EnsureContext() (*AuthContext, error) {
	c, err := h.readConfig()
	if err != nil {
		if errors.Is(err, ErrNoConfig) {
			return nil, nil
		}
		return nil, err
	}

	if len(c.SessionToken) > 0 {
		_, _ = fmt.Fprintf(h.verboseWriter, "You are signed in as <%s>.", c.IdentityTraits)
		if !h.noConfirm && !cmdx.AskForConfirmation("Press [y] to continue as that user or [n] to sign into another account.", os.Stdin, h.verboseWriter) {
			if err := h.SignOut(); err != nil {
				return nil, err
			}
			c = new(AuthContext)
			c, err = h.Authenticate()
			if err != nil {
				return nil, err
			}
		}

		return c, nil
	} else {
		c, err = h.Authenticate()
		if err != nil {
			return nil, err
		}
	}

	if len(c.SessionToken) > 0 && len(c.SelectedProject.String()) > 0 {
		return c, nil
	}

	return c, nil
}

func (h *Auth) getField(i interface{}, path string) (*gjson.Result, error) {
	var b bytes.Buffer
	if err := json.NewEncoder(&b).Encode(i); err != nil {
		return nil, err
	}
	result := gjson.GetBytes(b.Bytes(), path)
	return &result, nil
}

func (h *Auth) signup(c *kratos.APIClient) (*AuthContext, error) {
	flow, _, err := c.V0alpha2Api.InitializeSelfServiceRegistrationFlowWithoutBrowser(h.ctx).Execute()
	if err != nil {
		return nil, err
	}

	var isRetry bool
retryRegistration:
	if isRetry {
		_, _ = fmt.Fprintf(h.verboseWriter, "\nYour account creation attempt failed. Please try again!\n\n")
	}
	isRetry = true

	var form kratos.SubmitSelfServiceRegistrationFlowWithPasswordMethodBody
	if err := renderForm(os.Stdin, h.verboseWriter, flow.Ui, "password", &form); err != nil {
		return nil, err
	}

	signup, _, err := c.V0alpha2Api.SubmitSelfServiceRegistrationFlow(h.ctx).
		Flow(flow.Id).SubmitSelfServiceRegistrationFlowBody(kratos.SubmitSelfServiceRegistrationFlowBody{
		SubmitSelfServiceRegistrationFlowWithPasswordMethodBody: &form,
	}).Execute()
	if err != nil {
		if e, ok := err.(*kratos.GenericOpenAPIError); ok {
			switch m := e.Model().(type) {
			case *kratos.SelfServiceRegistrationFlow:
				flow = m
				goto retryRegistration
			case kratos.SelfServiceRegistrationFlow:
				flow = &m
				goto retryRegistration
			}
		}

		return nil, errors.WithStack(err)
	}

	sessionToken := *signup.SessionToken
	sess, _, err := c.V0alpha2Api.ToSession(h.ctx).XSessionToken(sessionToken).Execute()
	if err != nil {
		return nil, err
	}

	return h.sessionToContext(sess, sessionToken)
}

func (h *Auth) signin(c *kratos.APIClient, sessionToken string) (*AuthContext, error) {
	req := c.V0alpha2Api.InitializeSelfServiceLoginFlowWithoutBrowser(h.ctx)
	if len(sessionToken) > 0 {
		req = req.XSessionToken(sessionToken).Aal("aal2")
	}

	flow, _, err := req.Execute()
	if err != nil {
		return nil, err
	}

	var isRetry bool
retryLogin:
	if isRetry {
		_, _ = fmt.Fprintf(h.verboseWriter, "\nYour sign in attempt failed. Please try again!\n\n")
	}
	isRetry = true

	var form interface{} = &kratos.SubmitSelfServiceLoginFlowWithPasswordMethodBody{}
	method := "password"
	if len(sessionToken) > 0 {
		var foundTOTP bool
		var foundLookup bool
		for _, n := range flow.Ui.Nodes {
			if n.Group == "totp" {
				foundTOTP = true
			} else if n.Group == "lookup_secret" {
				foundLookup = true
			}
		}
		if !foundLookup && !foundTOTP {
			return nil, errors.New("only TOTP and lookup secrets are supported for two-step verification in the CLI")
		}

		method = "lookup_secret"
		if foundTOTP {
			form = &kratos.SubmitSelfServiceLoginFlowWithTotpMethodBody{}
			method = "totp"
		}
	}

	if err := renderForm(os.Stdin, h.verboseWriter, flow.Ui, method, form); err != nil {
		return nil, err
	}

	var body kratos.SubmitSelfServiceLoginFlowBody
	switch e := form.(type) {
	case *kratos.SubmitSelfServiceLoginFlowWithTotpMethodBody:
		body.SubmitSelfServiceLoginFlowWithTotpMethodBody = e
	case *kratos.SubmitSelfServiceLoginFlowWithPasswordMethodBody:
		body.SubmitSelfServiceLoginFlowWithPasswordMethodBody = e
	default:
		panic("unexpected type")
	}

	login, _, err := c.V0alpha2Api.SubmitSelfServiceLoginFlow(h.ctx).XSessionToken(sessionToken).
		Flow(flow.Id).SubmitSelfServiceLoginFlowBody(body).Execute()
	if err != nil {
		if e, ok := err.(*kratos.GenericOpenAPIError); ok {
			switch m := e.Model().(type) {
			case *kratos.SelfServiceLoginFlow:
				flow = m
				goto retryLogin
			case kratos.SelfServiceLoginFlow:
				flow = &m
				goto retryLogin
			}
		}

		return nil, errors.WithStack(err)
	}

	sessionToken = stringsx.Coalesce(*login.SessionToken, sessionToken)
	sess, _, err := c.V0alpha2Api.ToSession(h.ctx).XSessionToken(sessionToken).Execute()
	if err == nil {
		return h.sessionToContext(sess, sessionToken)
	}

	if e, ok := err.(*kratos.GenericOpenAPIError); ok {
		switch gjson.GetBytes(e.Body(), "error.id").String() {
		case "session_aal2_required":
			return h.signin(c, sessionToken)
		}
	}
	return nil, err
}

func (h *Auth) sessionToContext(session *kratos.Session, token string) (*AuthContext, error) {
	email, err := h.getField(session.Identity.Traits, "email")
	if err != nil {
		return nil, err
	}

	return &AuthContext{
		Version:      version,
		SessionToken: token,
		IdentityTraits: AuthIdentity{
			Email: email.String(),
			ID:    uuid.FromStringOrNil(session.Identity.Id),
		},
	}, nil
}

func (h *Auth) Authenticate() (*AuthContext, error) {
	if h.noConfirm {
		return nil, errors.New("can not sign in or sign up when flag --yes is set.")
	}

	ac, err := h.readConfig()
	if err != nil {
		if errors.Is(err, ErrNoConfig) {
			return nil, nil
		}
		return nil, err
	}

	if len(ac.SessionToken) > 0 {
		if !cmdx.AskForConfirmation(fmt.Sprintf("You are signed in as \"%s\" already. Do you wish to authenticate with another account?", ac.IdentityTraits.Email), os.Stdin, h.verboseWriter) {
			return ac, nil
		}

		_, _ = fmt.Fprintf(h.verboseWriter, "Ok, signing you out!\n")
		if err := h.SignOut(); err != nil {
			return nil, err
		}
	}

	c, err := newConsoleClient("public")
	if err != nil {
		return nil, err
	}

	signIn := cmdx.AskForConfirmation("Do you already have an Ory Console account you wish to use?", os.Stdin, h.verboseWriter)

	var retry bool
	if retry {
		_, _ = fmt.Fprintln(h.verboseWriter, "Unable to Authenticate you, please try again.")
	}

	if signIn {
		ac, err = h.signin(c, "")
		if err != nil {
			return nil, err
		}
	} else {
		ac, err = h.signup(c)
		if err != nil {
			return nil, err
		}
	}

	if err := h.WriteConfig(ac); err != nil {
		return nil, err
	}

	// List all the projects and select one
	//projects, _, err := c.V0alpha2Api.ListProjects(h.ctx).Execute()
	//if err != nil {
	//    return err
	//}
	//
	//for _, project := range projects {
	//    fmt.Printf("%s\n", project.Name)
	//}
	//
	//// Ask which project to use
	//var projectName string
	//fmt.Printf("Please select a project: ")
	//projectName, err := bufio.NewReader(os.Stdin).ReadString('\n')
	//if err != nil {
	//	return err
	//}

	_, _ = fmt.Fprintf(h.verboseWriter, "You are now signed in as: %s\n", ac.IdentityTraits.Email)

	return ac, nil
}

func (h *Auth) SignOut() error {
	return h.WriteConfig(new(AuthContext))
}

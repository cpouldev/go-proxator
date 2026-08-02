package proxator

import (
	"context"
	"net/http"

	"github.com/Noooste/azuretls-client"
)

type azureTLSSessionFactory struct{}

func defaultSessionFactory() SessionFactory {
	return azureTLSSessionFactory{}
}

func (azureTLSSessionFactory) New(proxyURL string) (Session, error) {
	session := azuretls.NewSession()
	session.Browser = azuretls.Chrome
	if err := session.SetProxy(proxyURL); err != nil {
		session.Close()
		return nil, err
	}
	return &azureTLSSession{session: session}, nil
}

type azureTLSSession struct {
	session *azuretls.Session
}

func (s *azureTLSSession) Do(ctx context.Context, req Request) (*Response, error) {
	s.session.SetContext(ctx)

	ordered := make(azuretls.OrderedHeaders, len(req.Headers))
	for i, header := range req.Headers {
		ordered[i] = append([]string(nil), header...)
	}

	resp, err := s.session.Do(&azuretls.Request{
		Method:         req.Method,
		Url:            req.URL,
		Body:           req.Body,
		OrderedHeaders: ordered,
	})
	if resp == nil {
		return nil, err
	}

	headers := make(http.Header, len(resp.Header))
	for key, values := range resp.Header {
		headers[key] = append([]string(nil), values...)
	}

	return &Response{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       append([]byte(nil), resp.Body...),
		Header:     headers,
	}, err
}

func (s *azureTLSSession) Close() {
	s.session.Close()
}

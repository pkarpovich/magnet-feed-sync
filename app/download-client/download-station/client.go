package download_station

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"magnet-feed-sync/app/config"
	"magnet-feed-sync/app/types"
	"net/http"
	"net/url"
	"strings"
)

type Client struct {
	baseUrl            string
	username           string
	password           string
	defaultDestination string
}

func NewClient(config config.SynologyConfig) *Client {
	return &Client{
		baseUrl:            config.URL,
		username:           config.Username,
		password:           config.Password,
		defaultDestination: config.Destination,
	}
}

type Session struct {
	SynoToken string
	Sid       string
}

func buildLoginUrl(baseUrl, account, password string) (string, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/webapi/auth.cgi", baseUrl), nil)
	if err != nil {
		return "", err
	}

	q := req.URL.Query()
	q.Add("api", "SYNO.API.Auth")
	q.Add("version", "3")
	q.Add("method", "login")
	q.Add("account", account)
	q.Add("passwd", password)
	q.Add("session", "DownloadStation")
	q.Add("format", "sid")
	q.Add("enable_syno_token", "yes")

	req.URL.RawQuery = q.Encode()

	return req.URL.String(), nil
}

func (c *Client) createSession() (*Session, error) {
	loginUrl, err := buildLoginUrl(c.baseUrl, c.username, c.password)
	if err != nil {
		return nil, err
	}

	resp, err := http.Get(loginUrl)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Printf("Error closing response body: %v", err)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err = json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	if success, ok := result["success"].(bool); !ok || !success {
		return nil, fmt.Errorf("API error: %v", result["error"])
	}

	data := result["data"].(map[string]interface{})
	return &Session{
		SynoToken: data["synotoken"].(string),
		Sid:       data["sid"].(string),
	}, nil
}

func (c *Client) CreateDownloadTask(magnet, destination string) error {
	session, err := c.createSession()
	if err != nil {
		return err
	}

	taskUrl := fmt.Sprintf("%s/webapi/entry.cgi", c.baseUrl)
	formData := url.Values{
		"api":         {"SYNO.DownloadStation2.Task"},
		"method":      {"create"},
		"version":     {"2"},
		"_sid":        {session.Sid},
		"destination": {destination},
		"type":        {"url"},
		"url":         {magnet},
		"create_list": {"false"},
	}

	var httpClient http.Client
	req, err := http.NewRequest("POST", taskUrl, strings.NewReader(formData.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-SYNO-TOKEN", session.SynoToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Printf("Error closing response body: %v", err)
		}
	}()

	respBodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	respBody := string(respBodyBytes)
	fmt.Println(respBody)

	return nil
}

func (c *Client) SetLocation(taskID, location string) error {
	log.Printf("[WARN] SetLocation not implemented for DownloadStation")
	return nil
}

func (c *Client) GetLocations() []types.Location {
	log.Printf("[WARN] GetLocations not implemented for DownloadStation")
	return nil
}

func (c *Client) GetHashByMagnet(magnet string) (string, error) {
	log.Printf("[WARN] GetHashByMagnet not implemented for DownloadStation")
	return "", nil
}

func (c *Client) GetDefaultLocation() string {
	return c.defaultDestination
}

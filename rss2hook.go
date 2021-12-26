// rss2hook is a simple utility which will make HTTP POST
// requests to remote web-hooks when new items appear in an RSS feed.
//
// Steve
//

package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/robfig/cron"
)

// RSSEntry describes a single RSS feed and the corresponding hook
// to POST to.
type RSSEntry struct {
	// The URL of the RSS/Atom feed
	feed string

	// The end-point to make the webhook request to.
	hook string
}

type Props struct {
	DeliveryMode int `json:"delivery_mode"`
}

type Payload struct {
	RSSItem *gofeed.Item `json:"item"`
	Source string `json:"source"`
}

type AMQP_Payload struct {
	Vhost   string `json:"vhost"`
	Name    string `json:"name"`
	RoutingKey string `json:"routing_key"`
	Properties Props `json:"properties"`
	Payload string `json:"payload"`
	Enc string `json:"payload_encoding"`
}

// Loaded contains the loaded feeds + hooks, as read from the specified
// configuration file
var Loaded []RSSEntry

// Timeout is the (global) timeout we use when loading remote RSS
// feeds.
var Timeout time.Duration

// Directory for storing seen items
var Seendir string
var Queuename string
var AMQPAccount string
var HTTPUsername string
var HTTPPassword string

// loadConfig loads the named configuration file and populates our
// `Loaded` list of RSS-feeds & Webhook addresses
func loadConfig(filename string) {
	file, err := os.Open(filename)
	if err != nil {
		fmt.Printf("Error opening %s - %s\n", filename, err.Error())
		return
	}
	defer file.Close()

	//
	// Process it line by line.
	//
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {

		// Get the next line, and strip leading/trailing space
		tmp := scanner.Text()
		tmp = strings.TrimSpace(tmp)

		//
		// Skip lines that begin with a comment.
		//
		if (tmp != "") && (!strings.HasPrefix(tmp, "#")) {

			//
			// Otherwise find the feed + post-point
			//
			parser := regexp.MustCompile("^(.*)=([^=]+)")
			match := parser.FindStringSubmatch(tmp)

			//
			// OK we found a suitable entry.
			//
			if len(match) == 3 {

				feed := strings.TrimSpace(match[1])
				hook := strings.TrimSpace(match[2])

				// Append the new entry to our list
				entry := RSSEntry{feed: feed, hook: hook}
				Loaded = append(Loaded, entry)
			}

		}
	}

}

// fetchFeed fetches the contents of the specified URL.
func fetchFeed(url string) (string, error) {

	// Ensure we setup a timeout for our fetch
	client := &http.Client{Timeout: Timeout}

	// We'll only make a GET request
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}

	// We ensure we identify ourself.
	req.Header.Set("User-Agent", "rss2email (https://github.com/skx/rss2email)")

	// Make the request
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Read the body returned
	output, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// isNew returns TRUE if this feed-item hasn't been notified about
// previously.
func isNew(parent string, item *gofeed.Item) bool {

	hasher := sha1.New()
	hasher.Write([]byte(parent))
	hasher.Write([]byte(item.GUID))
	hashBytes := hasher.Sum(nil)

	// Hexadecimal conversion
	hexSha1 := hex.EncodeToString(hashBytes)

	if _, err := os.Stat(Seendir + hexSha1); os.IsNotExist(err) {
		return true
	}
	return false
}

// recordSeen ensures that we won't re-announce a given feed-item.
func recordSeen(parent string, item *gofeed.Item) {

	hasher := sha1.New()
	hasher.Write([]byte(parent))
	hasher.Write([]byte(item.GUID))
	hashBytes := hasher.Sum(nil)

	// Hexadecimal conversion
	hexSha1 := hex.EncodeToString(hashBytes)

	dir := Seendir
	os.MkdirAll(dir, os.ModePerm)

	_ = ioutil.WriteFile(dir+"/"+hexSha1, []byte(item.Link), 0644)

}

// checkFeeds is our work-horse.
//
// For each available feed it looks for new entries, and when founds
// triggers `notify` upon the resulting entry
func checkFeeds() {
	client := &http.Client{}
	//
	// For each thing we're monitoring
	//
	for _, monitor := range Loaded {

		// Fetch the feed-contents
		content, err := fetchFeed(monitor.feed)

		if err != nil {
			fmt.Printf("Error fetching %s - %s\n",
				monitor.feed, err.Error())
			continue
		}

		// Now parse the feed contents into a set of items
		fp := gofeed.NewParser()
		feed, err := fp.ParseString(content)
		if err != nil {
			fmt.Printf("Error parsing %s contents: %s\n", monitor.feed, err.Error())
			continue
		}

		// For each entry in the feed
		for _, i := range feed.Items {

			// If we've not already notified about this one.
			if isNew(monitor.feed, i) {

				// Trigger the notification
				err := notify(client, monitor.feed, monitor.hook, i)

				// and if that notification succeeded
				// then record this item as having been
				// processed successfully.
				if err == nil {
					recordSeen(monitor.feed, i)
				}
			}
		}
	}
}

// notify actually submits the specified item to the remote webhook.
//
// The RSS-item is submitted as a JSON-object.
func notify(client *http.Client, feed string, hook string, item *gofeed.Item) error {
	
	// new AMQP_Payload 
	properties := Props{1}
	payload := Payload{
		item,
		feed,
	}
	// payload to json string
	jsonPayload, err := json.Marshal(payload)
	amqp_payload := AMQP_Payload{
		AMQPAccount, 
		"amq.default",
		Queuename, 
		properties,
		string(jsonPayload),
		"string",
	}

	// We'll post the item as a JSON object.
	// So first of all encode it.
	jsonValue, err := json.Marshal(amqp_payload)
	if err != nil {
		fmt.Printf("notify: Failed to encode JSON:%s\n", err.Error())
		return err
	}

	//
	// Post to the specified hook URL.
	//
	req, err := http.NewRequest("POST", hook, bytes.NewBuffer(jsonValue))
	// ...
	req.Header.Add("Content-Type", `application/json`)
	// if HTTPPassword or HTTPUsername are set, add them to the request
	if HTTPPassword != "" || HTTPUsername != "" {
		req.Header.Add("Authorization", `Basic ` + base64.StdEncoding.EncodeToString([]byte(HTTPUsername+":"+HTTPPassword)))
	}
	res, err := client.Do(req)
	if err != nil {
		fmt.Printf("notify: Failed to POST to %s - %s\n",
			hook, err.Error())
		return err
	}

	//
	// OK now we've submitted the post.
	//
	// We should retrieve the status-code + body, if the status-code
	// is "odd" then we'll show them.
	//
	defer res.Body.Close()
	_, err = ioutil.ReadAll(res.Body)
	if err != nil {
		return err
	}
	status := res.StatusCode

	if status != 200 {
		fmt.Printf("notify: Warning - Status code was not 200: %d\n", status)
	}
	return nil
}

// main is our entry-point
func main() {

	// Parse the command-line flags
	config := flag.String("config", "./rss.cfg", "The path to the configuration-file to read")
	seendir := flag.String("seendir", os.Getenv("HOME") + "/.rss2hook/seen/", "Path to thee Directory to store seen-items")
	queuename := flag.String("queuename", "", "The name of the queue to publish to")
	amqpaccount := flag.String("amqpaccount", "", "The name of the account to use for AMQP")
	httpusername := flag.String("username", "", "Username for HTTP Basic Auth")
	httppassword := flag.String("password", "", "Password for HTTP Basic Auth")
	timeout := flag.Duration("timeout", 5*time.Second, "The timeout used for fetching the remote feeds")
	flag.Parse()

	// Setup the default timeout.
	Timeout = *timeout
	Seendir = *seendir
	Queuename = *queuename
	AMQPAccount = *amqpaccount
	HTTPUsername = *httpusername
	HTTPPassword = *httppassword

	if *config == "" {
		fmt.Printf("Please specify a configuration-file to read\n")
		return
	}

	//
	// Load the configuration file
	//
	loadConfig(*config)

	//
	// Show the things we're monitoring
	//
	for _, ent := range Loaded {
		fmt.Printf("Monitoring feed %s\nPosting to %s\n\n",
			ent.feed, ent.hook)
	}

	//
	// Make the initial scan of feeds immediately to avoid waiting too
	// long for the first time.
	//
	checkFeeds()

	//
	// Now repeat that every five minutes.
	//
	c := cron.New()
	c.AddFunc("@every 5m", func() { checkFeeds() })
	c.Start()

	//
	// Now we can loop waiting to be terminated via ctrl-c, etc.
	//
	sigs := make(chan os.Signal, 1)
	done := make(chan bool, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		_ = <-sigs
		done <- true
	}()
	<-done
}

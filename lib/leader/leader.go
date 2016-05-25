package leader

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/gravitational/planet/lib/etcdconf"
	"github.com/gravitational/planet/lib/utils"
	"github.com/gravitational/trace"

	log "github.com/Sirupsen/logrus"
	"github.com/coreos/etcd/client"
	"github.com/mailgun/timetools"
	"golang.org/x/net/context"
)

// defaultResponseTimeout specifies the default time limit to wait for response
// header in a single request made by an etcd client
const defaultResponseTimeout = 1 * time.Second

// Config sets leader election configuration options
type Config struct {
	// ETCD defines etcd configuration
	ETCD etcdconf.Config
	// Clock is a time provider
	Clock timetools.TimeProvider
}

// Client implements ETCD-backed leader election client
// that helps to elect new leaders for a given key and
// monitors the changes to the leaders
type Client struct {
	client client.Client
	clock  timetools.TimeProvider
	closeC chan bool
	closed uint32
}

// NewClient returns a new instance of leader election client
func NewClient(cfg Config) (*Client, error) {
	if len(cfg.ETCD.Endpoints) == 0 {
		return nil, trace.Errorf("need at least one endpoint")
	}
	if cfg.Clock == nil {
		cfg.Clock = &timetools.RealTime{}
	}
	if cfg.ETCD.HeaderTimeoutPerRequest == 0 {
		cfg.ETCD.HeaderTimeoutPerRequest = defaultResponseTimeout
	}

	client, err := cfg.ETCD.NewClient()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &Client{
		client: client,
		clock:  cfg.Clock,
		closeC: make(chan bool),
	}, nil
}

// CallbackFn specifies callback that is called by AddWatchCallback
// whenever leader changes
type CallbackFn func(key, prevValue, newValue string)

// AddWatchCallback adds the given callback to be invoked when changes are
// made to the specified key's value. The callback is called with new and
// previous values for the key. In the first call, both values are the same
// and reflect the value of the key at that moment
func (l *Client) AddWatchCallback(key string, retry time.Duration, fn CallbackFn) {
	go func() {
		valuesC := make(chan string)
		l.AddWatch(key, retry, valuesC)
		var prev string
		for {
			select {
			case <-l.closeC:
				return
			case val := <-valuesC:
				fn(key, prev, val)
				prev = val
			}
		}
	}()
}

// AddWatch starts watching the key for changes and sending them
// to the valuesC, the watch is stopped
func (l *Client) AddWatch(key string, retry time.Duration, valuesC chan string) {
	prefix := fmt.Sprintf("AddWatch(key=%v)", key)
	api := client.NewKeysAPI(l.client)
	go func() {
		backoff := &utils.Backoff{
			Initial: 50 * time.Millisecond,
			Max:     10 * time.Second,
		}

		var watcher client.Watcher
		var re *client.Response
		var err error

		resetWatch := func() error {
			// make sure we've sent the existing value first,
			// so we can reliably detect the transitions
			re, err = l.getFirstValue(key, retry)
			if err != nil {
				log.Errorf("%v unexpected error: %v, returning", prefix, err)
				return err
			} else if re == nil {
				log.Infof("%v client is closing, return", prefix)
				return err
			}
			log.Infof("%v got current value '%v' for key '%v'", prefix, re.Node.Value, key)
			watcher = api.Watcher(key, &client.WatcherOptions{
				AfterIndex: re.Node.ModifiedIndex,
			})
			log.Infof("%v reset watch at %v", prefix, re.Node.ModifiedIndex)
			return nil
		}

		err = resetWatch()
		if err != nil {
			return
		}

		ctx, closer := context.WithCancel(context.Background())
		go func() {
			<-l.closeC
			closer()
		}()

		for {
			re, err = watcher.Next(ctx)
			if err == nil {
				if re.Node.Value == "" {
					log.Infof("watcher.Next for %v skipping empty value", key)
					continue
				}
				log.Infof("watcher.Next for %v got %v", key, re.Node.Value)
				backoff.Reset()
			}
			if err != nil {
				duration := backoff.Delay()
				if backoff.Tries > 1 {
					log.Infof("backing off for %v", duration)
					time.Sleep(duration)
				}

				if err == context.Canceled {
					log.Infof("client is closing, return")
					return
				} else if cerr, ok := err.(*client.ClusterError); ok {
					if len(cerr.Errors) != 0 && cerr.Errors[0] == context.Canceled {
						log.Infof("client is closing, return")
						return
					}
					log.Infof("unexpected cluster error: %v (%v)", err, cerr.Detail())
					continue
				} else if cerr, ok := err.(client.Error); ok && cerr.Code == client.ErrorCodeEventIndexCleared {
					log.Infof("watch index error, resetting watch index: %v", cerr)
					err = resetWatch()
					if err != nil {
						continue
					}
				} else {
					log.Infof("unexpected watch error: %v", err)
					// try recreating the watch if we get repeated unknown errors
					if backoff.Tries > 10 {
						resetWatch()
					}
					continue
				}
			}
			select {
			case valuesC <- re.Node.Value:
			case <-l.closeC:
				return
			}
		}
	}()
}

// AddVoter adds a voter that tries to elect given value
// by attempting to set the key to the value for a given term duration
// it also attempts to hold the lease indefinitely
func (l *Client) AddVoter(key, value string, term time.Duration) error {
	if value == "" {
		return trace.Errorf("voter value for key can not be empty")
	}
	if term < time.Second {
		return trace.Errorf("term can not be < 1second")
	}
	go func() {
		err := l.elect(key, value, term)
		if err != nil {
			log.Infof("voter error: %v", err)
		}
		ticker := time.NewTicker(term / 5)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				err := l.elect(key, value, term)
				if err != nil {
					log.Infof("voter error: %v", err)
				}
			case <-l.closeC:
				log.Infof("client is closing, return")
				return
			}
		}
	}()
	return nil
}

// getFirstValue returns the current value for key if it exists, or waits
// for the value to appear and loops until client.Close is called
func (l *Client) getFirstValue(key string, retryPeriod time.Duration) (*client.Response, error) {
	api := client.NewKeysAPI(l.client)
	tick := time.NewTicker(retryPeriod)
	defer tick.Stop()
	for {
		re, err := api.Get(context.TODO(), key, nil)
		if err == nil {
			return re, nil
		} else if !IsNotFound(err) {
			log.Infof("unexpected watcher error: %v", err)
		}
		select {
		case <-tick.C:
		case <-l.closeC:
			log.Infof("watcher got client close signal")
			return nil, nil
		}
	}
}

// elect is taken from: https://github.com/kubernetes/contrib/blob/master/pod-master/podmaster.go
// this is a slightly modified version though, that does not return the result
// instead we rely on watchers
func (l *Client) elect(key, value string, term time.Duration) error {
	candidate := fmt.Sprintf("candidate(key=%v, value=%v, term=%v)", key, value, term)
	log.Infof("%v start", candidate)
	api := client.NewKeysAPI(l.client)
	resp, err := api.Get(context.TODO(), key, nil)
	if err != nil {
		if !IsNotFound(err) {
			return trace.Wrap(err)
		}
		log.Infof("%v key not found, try to elect myself", candidate)
		// try to grab the lock for the given term
		_, err := api.Set(context.TODO(), key, value, &client.SetOptions{
			TTL:       term,
			PrevExist: client.PrevNoExist,
		})
		if err != nil {
			return trace.Wrap(err)
		}
		log.Infof("%v successfully elected", candidate)
		return nil
	}
	if resp.Node.Value != value {
		log.Infof("%v leader: is %v, try next time", candidate, resp.Node.Value)
		return nil
	}
	if resp.Node.Expiration.Sub(l.clock.UtcNow()) > time.Duration(term/2) {
		return nil
	}

	// extend the lease before the current expries
	_, err = api.Set(context.TODO(), key, value, &client.SetOptions{
		TTL:       term,
		PrevValue: value,
		PrevIndex: resp.Node.ModifiedIndex,
	})
	if err != nil {
		return trace.Wrap(err)
	}
	log.Infof("%v extended lease", candidate)
	return nil
}

// Close stops current operations and releases resources
func (l *Client) Close() error {
	// already closed
	if !atomic.CompareAndSwapUint32(&l.closed, 0, 1) {
		return nil
	}
	close(l.closeC)
	return nil
}

func IsNotFound(err error) bool {
	e, ok := err.(client.Error)
	if !ok {
		return false
	}
	return e.Code == client.ErrorCodeKeyNotFound
}

func IsAlreadyExist(err error) bool {
	e, ok := err.(client.Error)
	if !ok {
		return false
	}
	return e.Code == client.ErrorCodeNodeExist
}

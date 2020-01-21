// Copyright 2020 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package listener

import (
	"context"
	"errors"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/google/go-github/v28/github"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
	"sigs.k8s.io/controller-runtime/pkg/client"

	chnv1alpha1 "github.com/IBM/multicloud-operators-channel/pkg/apis/app/v1alpha1"
	appv1alpha1 "github.com/IBM/multicloud-operators-subscription/pkg/apis/app/v1alpha1"
)

const (
	defaultKeyFile   = "/etc/subscription/tls.key"
	defaultCrtFile   = "/etc/subscription/tls.crt"
	payloadFormParam = "payload"
	signatureHeader  = "X-Hub-Signature"
)

func (listener *WebhookListener) handleGithubWebhook(r *http.Request) error {
	var body []byte

	var signature string

	var event interface{}

	var err error

	body, signature, event, err = listener.ParseRequest(r)
	if err != nil {
		klog.Error("Failed to parse the request. error:", err)
		return err
	}

	subList := &appv1alpha1.SubscriptionList{}
	listopts := &client.ListOptions{}

	err = listener.LocalClient.List(context.TODO(), subList, listopts)
	if err != nil {
		klog.Error("Failed to get subscriptions. error: ", err)
		return err
	}

	// Loop through all subscriptions
	for _, sub := range subList.Items {
		klog.V(2).Info("Evaluating subscription: " + sub.GetName())

		chNamespace := ""
		chName := ""
		chType := ""

		if sub.Spec.Channel != "" {
			strs := strings.Split(sub.Spec.Channel, "/")
			if len(strs) == 2 {
				chNamespace = strs[0]
				chName = strs[1]
			} else {
				klog.Info("Failed to get channel namespace and name.")
				continue
			}
		}

		chkey := types.NamespacedName{Name: chName, Namespace: chNamespace}
		chobj := &chnv1alpha1.Channel{}
		err := listener.RemoteClient.Get(context.TODO(), chkey, chobj)

		if err == nil {
			chType = string(chobj.Spec.Type)
		} else {
			klog.Error("Failed to get subscription's channel. error: ", err)
			continue
		}

		// This WebHook event is applicable for this subscription if:
		// 		1. channel type is github
		// 		2. AND ValidateSignature is true with the channel's secret token
		// 		3. AND channel path contains the repo full name from the event
		// If these conditions are not met, skip to the next subscription.

		if !strings.EqualFold(chType, chnv1alpha1.ChannelTypeGitHub) {
			klog.V(2).Infof("The channel type is %s. Skipping to process this subscription.", chType)
			continue
		}

		if signature != "" {
			if !listener.validateSecret(signature, chobj.GetAnnotations(), chNamespace, body) {
				continue
			}
		}

		switch e := event.(type) {
		case *github.PullRequestEvent:
			if chobj.Spec.PathName == e.GetRepo().GetCloneURL() ||
				chobj.Spec.PathName == e.GetRepo().GetHTMLURL() ||
				chobj.Spec.PathName == e.GetRepo().GetURL() ||
				strings.Contains(chobj.Spec.PathName, e.GetRepo().GetFullName()) {
				klog.Info("Processing PUSH event from " + e.GetRepo().GetHTMLURL())
				listener.updateSubscription(sub)
			}
		case *github.PushEvent:
			if chobj.Spec.PathName == e.GetRepo().GetCloneURL() ||
				chobj.Spec.PathName == e.GetRepo().GetHTMLURL() ||
				chobj.Spec.PathName == e.GetRepo().GetURL() ||
				strings.Contains(chobj.Spec.PathName, e.GetRepo().GetFullName()) {
				klog.Info("Processing PUSH event from " + e.GetRepo().GetHTMLURL())
				listener.updateSubscription(sub)
			}
		default:
			klog.Infof("Unhandled event type %s\n", github.WebHookType(r))
			continue
		}
	}

	return nil
}

// ParseRequest parses incoming WebHook event request
func (listener *WebhookListener) ParseRequest(r *http.Request) (body []byte, signature string, event interface{}, err error) {
	var payload []byte

	switch contentType := r.Header.Get("Content-Type"); contentType {
	case "application/json":
		if body, err = ioutil.ReadAll(r.Body); err != nil {
			klog.Error("Failed to read the request body. error: ", err)
			return nil, "", nil, err
		}

		payload = body //the JSON payload
	case "application/x-www-form-urlencoded":
		if body, err = ioutil.ReadAll(r.Body); err != nil {
			klog.Error("Failed to read the request body. error: ", err)
			return nil, "", nil, err
		}

		form, err := url.ParseQuery(string(body))
		if err != nil {
			klog.Error("Failed to parse the request body. error: ", err)
			return nil, "", nil, err
		}

		payload = []byte(form.Get(payloadFormParam))
	default:
		klog.Warningf("Webhook request has unsupported Content-Type %q", contentType)
		return nil, "", nil, errors.New("Unsupported Content-Type: " + contentType)
	}

	defer r.Body.Close()

	signature = r.Header.Get(signatureHeader)

	event, err = github.ParseWebHook(github.WebHookType(r), payload)
	if err != nil {
		klog.Error("could not parse webhook. error:", err)
		return nil, "", nil, err
	}

	return body, signature, event, nil
}

func (listener *WebhookListener) validateSecret(signature string, annotations map[string]string, chNamespace string, body []byte) (ret bool) {
	secret := ""
	ret = true
	// Get GitHub WebHook secret from the channel annotations
	if annotations["webhook-secret"] == "" {
		klog.Info("No webhook secret found in annotations")

		ret = false
	} else {
		seckey := types.NamespacedName{Name: annotations["webhook-secret"], Namespace: chNamespace}
		secobj := &corev1.Secret{}

		err := listener.RemoteClient.Get(context.TODO(), seckey, secobj)
		if err != nil {
			klog.Info("Failed to get secret for channel webhook listener, error: ", err)
			ret = false
		}

		err = yaml.Unmarshal(secobj.Data["secret"], &secret)
		if err != nil {
			klog.Info("Failed to unmarshal secret from the webhook secret. Skip this subscription, error: ", err)
			ret = false
		} else if secret == "" {
			klog.Info("Failed to get secret from the webhook secret. Skip this subscription, error: ", err)
			ret = false
		}
	}
	// Using the channel's webhook secret, validate it against the request's body
	if err := github.ValidateSignature(signature, body, []byte(secret)); err != nil {
		klog.Info("Failed to validate webhook event signature, error: ", err)
		// If validation fails, this webhook event is not for this subscription. Skip.
		ret = false
	}

	return ret
}

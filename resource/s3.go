package resource

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Appliscale/tyr/configuration"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

type S3Bucket struct {
	*s3.Bucket
	S3Policy *S3Policy
	Region   *string
	*s3.ServerSideEncryptionConfiguration
	*s3.LoggingEnabled
}

type S3Buckets []*S3Bucket

type S3Policy struct {
	Version    string
	Id         string      `json:",omitempty"`
	Statements []Statement `json:"Statement"`
}

func NewS3Policy(s string) (*S3Policy, error) {
	b := []byte(s)
	s3Policy := &S3Policy{}
	err := json.Unmarshal(b, s3Policy)
	if err != nil {
		return nil, err
	}
	return s3Policy, nil
}

type Statement struct {
	Effect    string
	Principal Principal
	Actions   Actions `json:"Action"`
	Resource  string
	Condition Condition `json:",omitempty"`
}

type Condition struct {
	Bool map[string]string `json:",omitempty"`
	Null map[string]string `json:",omitempty"`
}

type Actions []string

func (a *Actions) UnmarshalJSON(b []byte) error {

	array := []string{}
	err := json.Unmarshal(b, &array)
	/*
		if error is: "json: cannot unmarshal string into Go value of type []string"
		then fallback to unmarshaling string
	*/
	if err != nil {
		s := ""
		err = json.Unmarshal(b, &s)
		if err != nil {
			return err
		}
		*a = append(*a, s)
		return nil
	}
	for _, action := range array {
		*a = append(*a, action)
	}
	return nil
}

// Principal : Specifies user, account, service or other
// 			   entity that is allowed or denied access to resource
type Principal struct {
	Map      map[string][]string // Values in Map: https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_elements_principal.html
	Wildcard string              // Values: *
}

func (p *Principal) UnmarshalJSON(b []byte) error {
	p.Map = make(map[string][]string)
	s := ""
	err := json.Unmarshal(b, &s)
	if err != nil {
		m := make(map[string]interface{})

		err = json.Unmarshal(b, &m)
		if err != nil {
			return err
		}
		for key, value := range m {
			switch t := value.(type) {
			case string:
				p.Map[key] = append(p.Map[key], value.(string))
			case []interface{}:
				for _, elem := range value.([]interface{}) {
					p.Map[key] = append(p.Map[key], elem.(string))
				}
			default:
				fmt.Printf("type: %T\n", t)
			}
		}
	}
	p.Wildcard = s
	return nil
}

func (b *S3Buckets) LoadRegions(sess *session.Session) error {
	sess.Handlers.Unmarshal.PushBackNamed(s3.NormalizeBucketLocationHandler)
	s3API := s3.New(sess)

	wg := sync.WaitGroup{}
	n := len(*b)
	wg.Add(n)
	done := make(chan bool, n)
	cerrs := make(chan error, n)

	go func() {
		wg.Wait()
		close(done)
		close(cerrs)
	}()

	for _, bucket := range *b {
		go func(s3Bucket *S3Bucket) {
			result, err := s3API.GetBucketLocation(&s3.GetBucketLocationInput{Bucket: s3Bucket.Name})
			if err != nil {
				cerrs <- err
				return
			}
			s3Bucket.Region = result.LocationConstraint
			done <- true
		}(bucket)
	}
	for i := 0; i < n; i++ {
		select {
		case <-done:
		case err := <-cerrs:
			return err
		}
	}

	return nil
}

// LoadNames : Get All S3 Bucket names
func (b *S3Buckets) LoadNames(sess *session.Session) error {
	s3API := s3.New(sess)

	result, err := s3API.ListBuckets(&s3.ListBucketsInput{})
	if err != nil {
		return err
	}
	for _, bucket := range result.Buckets {
		*b = append(*b, &S3Bucket{Bucket: bucket})
	}
	return nil
}

func getRegionMapOfS3APIs(s3Buckets S3Buckets, config *configuration.Config) (map[string]*s3.S3, error) {
	regionS3APIs := make(map[string]*s3.S3)
	for _, bucket := range s3Buckets {
		if _, ok := regionS3APIs[*bucket.Region]; !ok {
			sess, err := session.NewSessionWithOptions(
				session.Options{
					Config: aws.Config{
						Region: bucket.Region,
					},
					Profile: config.Profile,
				},
			)
			if err == nil {
				regionS3APIs[*bucket.Region] = s3.New(sess)
			} else {
				return nil, err
			}
		}
		// TODO : Add some check to stop iteration
		// if len(regionS3APIs) >= 17 {
		// 	break
		// }
	}
	return regionS3APIs, nil
}

func (b *S3Buckets) LoadFromAWS(sess *session.Session, config *configuration.Config) error {
	err := b.LoadNames(sess)
	if err != nil {
		return err
	}

	err = b.LoadRegions(sess)
	if err != nil {
		return err
	}

	regionS3APIs, err := getRegionMapOfS3APIs(*b, config)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	n := 3 * len(*b)
	done := make(chan bool, n)
	errs := make(chan error, n)
	wg.Add(n)

	go func() {
		wg.Wait()
		close(done)
		close(errs)
	}()

	for _, s3Bucket := range *b {
		regionS3API := regionS3APIs[*s3Bucket.Region]
		go getPolicy(s3Bucket, regionS3API, done, errs, &wg)
		go getEncryption(s3Bucket, regionS3API, done, errs, &wg)
		go getBucketLogging(s3Bucket, regionS3API, done, errs, &wg)
	}
	for i := 0; i < n; i++ {
		select {
		case <-done:
		case err := <-errs:
			return err
		}
	}
	return nil
}

func getPolicy(s3Bucket *S3Bucket, s3API *s3.S3, done chan bool, errc chan error, wg *sync.WaitGroup) {
	defer wg.Done()

	result, err := s3API.GetBucketPolicy(&s3.GetBucketPolicyInput{
		Bucket: s3Bucket.Name,
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "NoSuchBucketPolicy":
				done <- true
			default:
				errc <- fmt.Errorf("[AWS-ERROR] Bucket: %s  Error Msg: %s", *s3Bucket.Name, aerr.Error())
			}
		} else {
			errc <- fmt.Errorf("[ERROR] %s: %s", *s3Bucket.Name, err.Error())
		}
		return
	}
	if result.Policy != nil {
		s3Bucket.S3Policy, err = NewS3Policy(*result.Policy)
		if err != nil {
			errc <- fmt.Errorf("[ERROR] Bucket: %s Error Msg: %s", *s3Bucket.Name, err.Error())
			return
		}
	}
	done <- true
}

func getEncryption(s3Bucket *S3Bucket, s3API *s3.S3, done chan bool, errs chan error, wg *sync.WaitGroup) {
	defer wg.Done()
	result, err := s3API.GetBucketEncryption(&s3.GetBucketEncryptionInput{Bucket: s3Bucket.Name})

	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "ServerSideEncryptionConfigurationNotFoundError":
				done <- true
			default:
				errs <- fmt.Errorf("[AWS-ERROR] \nBucket: %s \n Error Msg: %s", *s3Bucket.Name, aerr.Error())
			}
		} else {
			errs <- fmt.Errorf("[ERROR] %s: %s", *s3Bucket.Name, err.Error())
		}
		return
	}

	if result.ServerSideEncryptionConfiguration != nil {
		s3Bucket.ServerSideEncryptionConfiguration = result.ServerSideEncryptionConfiguration
	}
	done <- true
}

func getBucketLogging(s3Bucket *S3Bucket, s3API *s3.S3, done chan bool, errs chan error, wg *sync.WaitGroup) {
	defer wg.Done()
	result, err := s3API.GetBucketLogging(&s3.GetBucketLoggingInput{Bucket: s3Bucket.Name})
	if err != nil {
		errs <- fmt.Errorf("[ERROR] %s: %s", *s3Bucket.Name, err.Error())
		return
	}
	s3Bucket.LoggingEnabled = result.LoggingEnabled
	done <- true
}

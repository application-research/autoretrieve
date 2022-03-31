# Updating the Docker image on EC2

Autoretrieve uses a private AWS Elastic Container Registry (ECR) to store autoretrieve images. The process involves uploading to the regristry, logging into the EC2 machine, pulling the latest image, and then running it.

## Prerequisites
- AWS credentials (with permission to push to ECR)
- [AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/cli-chap-welcome.html)
- [Docker](https://docs.docker.com/get-docker/)

## Uploading to ECR
Get an Elastic ECR login token and pipe it to the `docker login` command.

**NOTE:** This will use the currently configured AWS profile. For more details about running the AWS CLI with different profiles, see the [AWS CLI documentation on using name profiles](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-profiles.html#using-profiles).
```
$ aws ecr get-login-password --region us-east-2 | docker login --username AWS --password-stdin 407967248065.dkr.ecr.us-east-2.amazonaws.com

Login Succeeded
```

Build the image locally using [`docker build`](https://docs.docker.com/engine/reference/commandline/build/).

**NOTE:** The image takes advantage of [Docker BuildKit](https://docs.docker.com/develop/develop-images/build_enhancements/) to speed up builds. It is recommended to use the `DOCKER_BUILDKIT=1` 
```
$ DOCKER_BUILDKIT=1 docker build -t autoretreive .
```

Tag the image using [`docker tag`](https://docs.docker.com/engine/reference/commandline/tag/).

**NOTE:** To push an image to a private registry, and not the central Docker registry, we must tag it with the registry hostname. In this case, the host name is the ECR repository URI, **407967248065.dkr.ecr.us-east-2.amazonaws.com**.
```
$ docker tag autoretrieve:latest 407967248065.dkr.ecr.us-east-2.amazonaws.com/autoretrieve:latest
```

Push the image to the ECR repository using [`docker push`](https://docs.docker.com/engine/reference/commandline/push/).
```
$ docker push 407967248065.dkr.ecr.us-east-2.amazonaws.com/autoretrieve:latest
```

## Updating the image on an EC2 instance

**Note:** The machine needs to have been setup with the [Docker Credential Helper for ECR](https://github.com/awslabs/amazon-ecr-credential-helper) prior to running the following command.

The following command will pull the latest autoretrieve docker image from ECR using [`docker run`](https://docs.docker.com/engine/reference/commandline/run/).
```
$ docker pull 407967248065.dkr.ecr.us-east-2.amazonaws.com/autoretrieve:latest
```

The following command will run the docker image. The environment variables passed in are configuring the autoretrieve instance, and can be substituted for any valid configuration value for the given argument. We use the `DOCKER_BUILDKIT` environment variable for speedy runs.
```
$ DOCKER_BUILDKIT=1 docker run -e FULLNODE_API_INFO=wss://api.chain.love -e AUTORETRIEVE_ENDPOINT_TYPE=indexer -e AUTORETRIEVE_ENDPOINT=https://cid.contact -e AUTORETRIEVE_PRUNE_THRESHOLD=100000000000 -e GOLOG_FILE=data/logs/logs.txt -e GOLOG_OUTPUT=stdout+file -v /storage/autoretrieve/data:/app/data -p 6746:6746 -p 8080:8080 --rm -it 407967248065.dkr.ecr.us-east-2.amazonaws.com/autoretrieve:latest
```
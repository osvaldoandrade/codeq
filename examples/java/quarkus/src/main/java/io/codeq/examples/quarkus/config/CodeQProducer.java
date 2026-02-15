package io.codeq.examples.quarkus.config;

import io.codeq.sdk.CodeQClient;
import jakarta.enterprise.context.ApplicationScoped;
import jakarta.enterprise.inject.Produces;
import org.eclipse.microprofile.config.inject.ConfigProperty;

/**
 * CDI producer for CodeQ client.
 * Configures and provides CodeQClient as injectable bean.
 */
@ApplicationScoped
public class CodeQProducer {

    @ConfigProperty(name = "codeq.base-url")
    String baseUrl;

    @ConfigProperty(name = "codeq.producer-token")
    String producerToken;

    @ConfigProperty(name = "codeq.worker-token")
    String workerToken;

    /**
     * Produces CodeQ client bean.
     * 
     * @return Configured CodeQClient instance
     */
    @Produces
    @ApplicationScoped
    public CodeQClient codeQClient() {
        return CodeQClient.builder()
                .baseUrl(baseUrl)
                .producerToken(producerToken)
                .workerToken(workerToken)
                .build();
    }
}

package io.codeq.examples.springboot.config;

import io.codeq.sdk.CodeQClient;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;

/**
 * Configuration for CodeQ client.
 * 
 * Reads configuration from application.properties and creates
 * a singleton CodeQClient bean for dependency injection.
 */
@Configuration
public class CodeQConfig {

    @Value("${codeq.base-url}")
    private String baseUrl;

    @Value("${codeq.producer-token}")
    private String producerToken;

    @Value("${codeq.worker-token}")
    private String workerToken;

    /**
     * Creates CodeQ client bean.
     * 
     * @return Configured CodeQClient instance
     */
    @Bean
    public CodeQClient codeQClient() {
        return CodeQClient.builder()
                .baseUrl(baseUrl)
                .producerToken(producerToken)
                .workerToken(workerToken)
                .build();
    }
}
